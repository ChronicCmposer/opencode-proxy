// Package tunnel establishes the single multiplexed connection the local
// proxy dials outbound to the remote proxy, and turns it into a yamux
// session usable as a net.Listener (local side) or a stream dialer (remote
// side).
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

// Path is the fixed HTTP path the remote proxy reserves for tunnel upgrades.
// Every other path is treated as a request to forward to opencode.
const Path = "/_tunnel"

// Config is the shared yamux tuning. Read/write deadlines are left disabled
// and StreamOpenTimeout is generous because a browser's GET /event SSE
// stream is expected to sit idle-but-open for a long time.
func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 30 * time.Second
	c.StreamOpenTimeout = 2 * time.Minute
	return c
}

// Dial opens one WSS connection to remoteURL (e.g. "wss://code.example.com/_tunnel")
// authenticated with tlsConf, and wraps it as a yamux client session. The
// caller owns the returned session's lifetime and must Close it.
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

// Accept upgrades an incoming HTTP request at Path to a WebSocket and wraps
// it as a yamux server session. The caller must have already verified the
// peer's client certificate carries the tunnel role before calling this.
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
