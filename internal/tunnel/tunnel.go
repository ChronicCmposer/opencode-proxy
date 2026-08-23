// Package tunnel wraps the outbound WSS connection as a yamux session:
// yamux.Session satisfies both net.Listener and a stream dialer, which is
// what lets local (internal/local) and remote (internal/remote) reuse it as
// the transport for a normal http.Server / http.Transport respectively.
package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const Path = "/_tunnel"

// StreamOpenTimeout is generous because a browser's GET /event SSE stream
// is expected to sit idle-but-open for a long time.
func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 30 * time.Second
	c.StreamOpenTimeout = 2 * time.Minute
	return c
}

// Dial's caller owns the returned session's lifetime and must Close it.
func Dial(ctx context.Context, remoteURL string, tlsConf *tls.Config) (*yamux.Session, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConf},
	}
	c, _, err := websocket.Dial(ctx, remoteURL, &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("dial tunnel: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	sess, err := yamux.Client(conn, Config())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start yamux client: %w", err)
	}
	return sess, nil
}

// Accept assumes the caller has already verified the peer's client
// certificate carries the tunnel role — it does no such check itself.
func Accept(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("accept tunnel upgrade: %w", err)
	}
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	sess, err := yamux.Server(conn, Config())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start yamux server: %w", err)
	}
	return sess, nil
}
