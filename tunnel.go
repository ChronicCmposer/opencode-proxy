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

const TunnelPath = "/_tunnel"

// YamuxConfigFactory builds a fresh yamux.Config for each tunnel session:
// DialTunnel/AcceptTunnel are called repeatedly across reconnects and newly
// accepted connections, and a yamux.Config must not be shared across
// sessions.
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

// DialTunnel's caller owns the returned session's lifetime and must Close it.
func DialTunnel(ctx context.Context, remoteURL string, dialerFactory TunnelDialerFactory, yamuxConfigFactory YamuxConfigFactory) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: dialerFactory(),
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return NewYamuxSession(conn, yamuxConfigFactory(), yamuxRoleClient)
}

// AcceptTunnel assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func AcceptTunnel(w http.ResponseWriter, r *http.Request, yamuxConfigFactory YamuxConfigFactory) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return NewYamuxSession(conn, yamuxConfigFactory(), yamuxRoleServer)
}

// yamuxRole selects yamux.Client vs yamux.Server in NewYamuxSession.
type yamuxRole int

const (
	yamuxRoleClient yamuxRole = iota
	yamuxRoleServer
)

func (r yamuxRole) String() string {
	if r == yamuxRoleServer {
		return "server"
	}
	return "client"
}

// serveTunnelSession runs run on sess in a goroutine until it finishes or
// ctx is cancelled, closing sess either way. err is run's result, and is
// always nil when cancelled.
func serveTunnelSession(ctx context.Context, sess *yamux.Session, run func() error) (cancelled bool, err error) {
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() { errCh <- run(); close(done) }()
	cancelled = waitOrCancel(ctx, done, func() { sess.Close() })
	sess.Close()
	if cancelled {
		return true, nil
	}
	return false, <-errCh
}

// awaitTunnelSession waits for sess to close on its own or for ctx to be
// cancelled, closing sess either way. The accept side has nothing to drive:
// the tunnel client is what's serving.
func awaitTunnelSession(ctx context.Context, sess *yamux.Session) (cancelled bool) {
	cancelled = waitOrCancel(ctx, sess.CloseChan(), func() { sess.Close() })
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
func NewYamuxSession(conn net.Conn, cfg *yamux.Config, role yamuxRole) (*yamux.Session, error) {
	var sess *yamux.Session
	var err error
	if role == yamuxRoleServer {
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
