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
type YamuxConfigFactory struct{}

func NewYamuxConfigFactory() *YamuxConfigFactory {
	return &YamuxConfigFactory{}
}

// CreateConfig's StreamOpenTimeout is generous because a browser's GET
// /event SSE stream is expected to sit idle-but-open for a long time.
func (f *YamuxConfigFactory) CreateConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 30 * time.Second
	c.StreamOpenTimeout = 2 * time.Minute
	return c
}

// TunnelDialerFactory builds a fresh http.Client for each dial attempt, since
// LocalClient.Run calls DialTunnel repeatedly across reconnects and a client
// with a broken Transport must not be reused.
type TunnelDialerFactory struct {
	tlsConf *tls.Config
}

func NewTunnelDialerFactory(tlsConf *tls.Config) *TunnelDialerFactory {
	return &TunnelDialerFactory{tlsConf: tlsConf}
}

func (f *TunnelDialerFactory) CreateClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: f.tlsConf},
	}
}

// DialTunnel's caller owns the returned session's lifetime and must Close it.
func DialTunnel(ctx context.Context, remoteURL string, dialers *TunnelDialerFactory, yamuxConfigs *YamuxConfigFactory) (*yamux.Session, error) {
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: dialers.CreateClient(),
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return newYamuxSession(conn, yamuxConfigs.CreateConfig(), true, "client")
}

// AcceptTunnel assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func AcceptTunnel(w http.ResponseWriter, r *http.Request, yamuxConfigs *YamuxConfigFactory) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return newYamuxSession(conn, yamuxConfigs.CreateConfig(), false, "server")
}

// newYamuxSession wraps conn (already an established websocket connection)
// as a yamux session, closing conn on failure. asClient selects yamux.Client
// vs yamux.Server; role only affects the wrapped error message.
func newYamuxSession(conn net.Conn, cfg *yamux.Config, asClient bool, role string) (*yamux.Session, error) {
	var sess *yamux.Session
	var err error
	if asClient {
		sess, err = yamux.Client(conn, cfg)
	} else {
		sess, err = yamux.Server(conn, cfg)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start yamux %s: %w", role, err)
	}
	return sess, nil
}
