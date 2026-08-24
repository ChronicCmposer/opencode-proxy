// The outbound WSS connection wrapped as a yamux session: yamux.Session
// satisfies both net.Listener and a stream dialer, which is what lets
// LocalClient and RemoteServer reuse it as the transport for a normal
// http.Server / http.Transport respectively.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// NewYamuxConfig builds the yamux.Config every tunnel session is
// constructed from.
//
// A single instance is safe to share, even across sessions running fully
// concurrently — which AcceptTunnel's callers can do, since net/http.Server
// serves each accepted connection on its own goroutine and nothing here
// serializes entry into AcceptTunnel. hashicorp/yamux's Session stores the
// *Config pointer it's given and reads from it for its whole life (keepalive
// interval, stream timeouts, window size, ...), from several goroutines, but
// never writes back into it after construction — confirmed by reading the
// package source. Concurrent reads of memory nothing ever writes to again
// aren't a data race.
func NewYamuxConfig(keepAliveInterval, streamOpenTimeout time.Duration) *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = keepAliveInterval
	c.StreamOpenTimeout = streamOpenTimeout
	return c
}

// NewTunnelDialer builds the http.Client DialTunnel uses to reach the
// remote: see DialTunnel's doc comment for why a single instance is safe to
// share across every dial attempt.
func NewTunnelDialer(tlsConf *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConf},
	}
}

// TunnelFactory builds tunnel sessions for both halves of the proxy. It holds
// the yamuxConfig and netConnCtx both DialTunnel and AcceptTunnel need, so
// callers don't have to thread them through separately at every call site.
type TunnelFactory struct {
	yamuxConfig *yamux.Config
	// netConnCtx bounds the lifetime of every wrapped connection's Read/Write
	// (websocket.NetConn's doc comment: "If cancelled, all reads and writes
	// on the net.Conn will be cancelled") — deliberately not DialTunnel's own
	// ctx parameter, which only bounds the dial itself; tying the connection's
	// whole lifetime to that too would be a second, redundant cancellation
	// source, since teardown already happens explicitly via sess.Close()
	// (see closeOnCancel/waitForTunnelClose). context.Background() never
	// cancels, so nothing but that explicit Close ever ends a session's
	// reads/writes. Sharing one instance across sessions is safe regardless:
	// NetConn derives a fresh child context per call
	// (context.WithCancel(netConnCtx)), whether the parent passed in is
	// freshly created or shared — nothing here is session-specific to begin
	// with.
	netConnCtx context.Context
}

func NewTunnelFactory(yamuxConfig *yamux.Config, netConnCtx context.Context) *TunnelFactory {
	return &TunnelFactory{yamuxConfig: yamuxConfig, netConnCtx: netConnCtx}
}

// DialTunnel's caller owns the returned session's lifetime and must Close it.
// dialer stays a parameter rather than a TunnelFactory field: only the dial
// side ever needs one, and storing it here would leave the accept side
// holding a meaningless nil field.
//
// dialer is safe to share across repeated calls, unlike most http.Clients
// reused past a broken Transport: websocket.Dial below only ever calls
// dialer.Do once, relying on net/http's built-in 101 Switching Protocols
// handling — the upgraded connection comes back via the response body and is
// dropped from the Transport's own pool, so neither a failed nor a
// successful dial leaves it broken for the next attempt.
func (f *TunnelFactory) DialTunnel(ctx context.Context, remoteURL string, dialer *http.Client) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: dialer,
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	return f.wrapSession(c, YamuxRoleClient)
}

// AcceptTunnel assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func (f *TunnelFactory) AcceptTunnel(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	return f.wrapSession(c, YamuxRoleServer)
}

func (f *TunnelFactory) wrapSession(c *websocket.Conn, role YamuxRole) (*yamux.Session, error) {
	conn := websocket.NetConn(f.netConnCtx, c, websocket.MessageBinary)
	return NewYamuxSession(conn, f.yamuxConfig, role)
}

// YamuxRole selects yamux.Client vs yamux.Server in NewYamuxSession.
type YamuxRole int

const (
	YamuxRoleClient YamuxRole = iota
	YamuxRoleServer
)

func (r YamuxRole) String() string {
	if r == YamuxRoleServer {
		return "server"
	}
	return "client"
}

// runTunnelSession runs run on sess in a goroutine until it finishes or ctx
// is cancelled, closing sess either way. err is run's result, and is always
// nil when cancelled. This is the dial/client side's driver: it actively
// runs something (the local HTTP server) over the session.
func runTunnelSession(ctx context.Context, sess *yamux.Session, run func() error) (cancelled bool, err error) {
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() { errCh <- run(); close(done) }()
	if closeOnCancel(ctx, sess, done) {
		return true, nil
	}
	return false, <-errCh
}

// waitForTunnelClose waits for sess to close on its own or for ctx to be
// cancelled, closing sess either way. This is the accept/server side's
// observer: it has nothing to drive, since the tunnel client is what's
// serving.
func waitForTunnelClose(ctx context.Context, sess *yamux.Session) (cancelled bool) {
	return closeOnCancel(ctx, sess, sess.CloseChan())
}

// closeOnCancel waits for done to fire or ctx to be cancelled, closing sess
// either way, and reports whether cancellation is what ended the wait.
// runTunnelSession and waitForTunnelClose both need exactly this, against
// different done channels.
func closeOnCancel(ctx context.Context, sess *yamux.Session, done <-chan struct{}) bool {
	cancelled := waitOrCancel(ctx, done, func() { sess.Close() })
	sess.Close()
	return cancelled
}

// waitOrCancel waits for ctx to be cancelled or done to fire, reporting
// which. On cancellation it calls onCancel to unblock done's producer, then
// waits for done so that goroutine can't leak.
func waitOrCancel(ctx context.Context, done <-chan struct{}, onCancel func()) bool {
	select {
	case <-ctx.Done():
		onCancel()
		<-done
		return true
	case <-done:
		return false
	}
}

// raceCtx runs work in a goroutine and returns as soon as either it finishes
// or ctx is cancelled, whichever comes first. Unlike waitOrCancel, work has
// no way to be aborted early (there's nothing to call an onCancel on): a
// cancellation just stops raceCtx from waiting on it any longer, while the
// goroutine itself runs to completion and its result is discarded.
func raceCtx[T any](ctx context.Context, work func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		val, err := work()
		ch <- result{val, err}
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case r := <-ch:
		return r.val, r.err
	}
}

// NewYamuxSession wraps conn (already an established websocket connection)
// as a yamux session, closing conn on failure.
func NewYamuxSession(conn net.Conn, cfg *yamux.Config, role YamuxRole) (*yamux.Session, error) {
	var sess *yamux.Session
	var err error
	if role == YamuxRoleServer {
		sess, err = yamux.Server(conn, cfg)
	} else {
		sess, err = yamux.Client(conn, cfg)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start yamux %s: %w", role, err)
	}
	return sess, nil
}
