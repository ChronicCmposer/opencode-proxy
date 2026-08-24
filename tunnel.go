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

// NewYamuxConfigFactory's returned factory has a generous StreamOpenTimeout
// because a browser's GET /event SSE stream is expected to sit idle-but-open
// for a long time.
func NewYamuxConfigFactory() YamuxConfigFactory {
	return func() *yamux.Config {
		c := yamux.DefaultConfig()
		c.EnableKeepAlive = true
		c.KeepAliveInterval = 30 * time.Second
		c.StreamOpenTimeout = 2 * time.Minute
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
func DialTunnel(ctx context.Context, remoteURL string, dialers TunnelDialerFactory, yamuxConfigs YamuxConfigFactory) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: dialers(),
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return newYamuxSession(conn, yamuxConfigs(), yamuxRoleClient)
}

// AcceptTunnel assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func AcceptTunnel(w http.ResponseWriter, r *http.Request, yamuxConfigs YamuxConfigFactory) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return newYamuxSession(conn, yamuxConfigs(), yamuxRoleServer)
}

// yamuxRole selects yamux.Client vs yamux.Server in newYamuxSession. Using an
// enum instead of a raw string rules out an unrecognized-role case at
// compile time, since the only two callers are DialTunnel and AcceptTunnel
// above.
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

// runTunnelSession runs run (if non-nil) on sess in a goroutine and waits for
// it to finish or ctx to be cancelled first, closing sess either way; if run
// is nil it just waits for sess to close on its own (the accept side has
// nothing to drive — the tunnel client is what's serving). This is the
// cancel-or-finish shutdown dance shared by LocalClient.Run (which serves an
// http.Server over sess) and acceptTunnel (which only waits on it), so both
// ends handle ctx cancellation identically. cancelled reports whether ctx
// cancellation is why it returned; err is run's result, or nil when run is
// nil or cancelled is true.
func runTunnelSession(ctx context.Context, sess *yamux.Session, run func() error) (cancelled bool, err error) {
	done := sess.CloseChan()
	if run != nil {
		ch := make(chan struct{})
		go func() { err = run(); close(ch) }()
		done = ch
	}
	cancelled = waitOrCancel(ctx, done, func() { sess.Close() })
	sess.Close()
	if cancelled {
		return true, nil
	}
	return false, err
}

// newYamuxSession wraps conn (already an established websocket connection)
// as a yamux session, closing conn on failure.
func newYamuxSession(conn net.Conn, cfg *yamux.Config, role yamuxRole) (*yamux.Session, error) {
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
