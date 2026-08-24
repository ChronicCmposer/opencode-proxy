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

// YamuxConfigFactory builds a fresh yamux.Config for each tunnel session:
// TunnelFactory's DialTunnel/AcceptTunnel are called repeatedly across
// reconnects and newly accepted connections, and a yamux.Config must not be
// shared across sessions.
type YamuxConfigFactory func() *yamux.Config

func NewYamuxConfigFactory(keepAliveInterval, streamOpenTimeout time.Duration) YamuxConfigFactory {
	return func() *yamux.Config {
		c := yamux.DefaultConfig()
		c.EnableKeepAlive = true
		c.KeepAliveInterval = keepAliveInterval
		c.StreamOpenTimeout = streamOpenTimeout
		return c
	}
}

// TunnelDialerFactory builds a fresh http.Client for each dial attempt, since
// LocalClient.Run calls DialTunnel repeatedly across reconnects and a client
// with a broken Transport must not be reused.
type TunnelDialerFactory func() *http.Client

func NewTunnelDialerFactory(tlsConf *tls.Config) TunnelDialerFactory {
	return func() *http.Client {
		return &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsConf},
		}
	}
}

// TunnelFactory builds tunnel sessions for both halves of the proxy. It holds
// the yamuxConfigFactory both DialTunnel and AcceptTunnel need, so callers
// don't have to thread it through separately at every call site.
type TunnelFactory struct {
	yamuxConfigFactory YamuxConfigFactory
}

func NewTunnelFactory(yamuxConfigFactory YamuxConfigFactory) *TunnelFactory {
	return &TunnelFactory{yamuxConfigFactory: yamuxConfigFactory}
}

// DialTunnel's caller owns the returned session's lifetime and must Close it.
// dialerFactory stays a parameter rather than a TunnelFactory field: only the
// dial side ever needs one, and storing it here would leave the accept side's
// factory holding a meaningless nil field.
func (f *TunnelFactory) DialTunnel(ctx context.Context, remoteURL string, dialerFactory TunnelDialerFactory) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: dialerFactory(),
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return NewYamuxSession(conn, f.yamuxConfigFactory(), YamuxRoleClient)
}

// AcceptTunnel assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func (f *TunnelFactory) AcceptTunnel(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return NewYamuxSession(conn, f.yamuxConfigFactory(), YamuxRoleServer)
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
