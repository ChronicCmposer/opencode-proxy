// The local half of the proxy: it dials the remote tunnel outbound,
// reconnecting with backoff, and reverse-proxies every request that
// arrives over it to opencode's local HTTP server.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const LocalVersionHeader = "X-Opencode-Proxy-Local-Version"

type LocalOptions struct {
	RemoteURL string
	Logger    *log.Logger
}

type LocalClient struct {
	opts         LocalOptions
	log          *log.Logger
	proxy        *httputil.ReverseProxy
	dialers      TunnelDialerFactory
	servers      LocalServerFactory
	yamuxConfigs YamuxConfigFactory
	backoff      *Backoff
}

// LocalServerFactory builds a fresh http.Server for each reconnect: Run
// serves a new tunnel session every time, and http.Server must not be reused
// across sessions once it has been Serve'd and stopped.
type LocalServerFactory func() *http.Server

func NewLocalServerFactory(handler http.Handler) LocalServerFactory {
	return func() *http.Server {
		return &http.Server{Handler: handler}
	}
}

// NewLocalProxy builds the reverse proxy to opencode's local HTTP server.
func NewLocalProxy(opencodeURL string, l *log.Logger) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(opencodeURL)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		// Rewrite, not Director: it strips the client's X-Forwarded-*
		// headers instead of appending to them.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host // SetURL clears Out.Host
		},
		// Not DefaultTransport: no env-var proxying, no idle timeout.
		Transport:     &http.Transport{},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Printf("opencode proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "opencode unreachable", http.StatusBadGateway)
		},
	}, nil
}

func NewLocalClient(opts LocalOptions, proxy *httputil.ReverseProxy, dialers TunnelDialerFactory, servers LocalServerFactory, yamuxConfigs YamuxConfigFactory, backoff *Backoff) *LocalClient {
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	return &LocalClient{
		opts:         opts,
		log:          l,
		proxy:        proxy,
		dialers:      dialers,
		servers:      servers,
		yamuxConfigs: yamuxConfigs,
		backoff:      backoff,
	}
}

// Run dials the tunnel and serves it until ctx is cancelled, reconnecting
// with backoff in between. Cancellation is the expected way to stop, so it
// returns nil rather than ctx.Err(), leaving the process exit code 0 on a
// clean SIGTERM.
func (c *LocalClient) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		sess, err := DialTunnel(ctx, c.opts.RemoteURL, c.dialers, c.yamuxConfigs)
		if err != nil {
			c.log.Printf("tunnel dial failed: %v", err)
			if !c.waitToRetry(ctx) {
				break
			}
			continue
		}
		c.log.Printf("tunnel connected to %s", c.opts.RemoteURL)
		c.backoff.Reset()

		srv := c.servers()
		cancelled, serveErr := serveTunnelSession(ctx, sess, func() error { return srv.Serve(sess) })
		if cancelled {
			break
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			c.log.Printf("tunnel session ended: %v", serveErr)
		}

		if !c.waitToRetry(ctx) {
			break
		}
	}
	return nil
}

func (c *LocalClient) waitToRetry(ctx context.Context) bool {
	return sleepCtx(ctx, c.backoff.Next())
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
