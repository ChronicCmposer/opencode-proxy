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
// It's split out of NewLocalClient so main can build it first, wrap it in
// LocalWithVersionHeader, and hand the result to NewLocalServerFactory —
// letting every dependency reach NewLocalClient through its constructor
// instead of being patched in afterward.
func NewLocalProxy(opencodeURL string, l *log.Logger) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(opencodeURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	base := proxy.Director
	proxy.Director = func(r *http.Request) {
		base(r)
		r.Host = target.Host
	}
	proxy.Transport = &http.Transport{
		ResponseHeaderTimeout: 0,
		IdleConnTimeout:       0,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		l.Printf("opencode proxy error for %s: %v", r.URL.Path, err)
		http.Error(w, "opencode unreachable", http.StatusBadGateway)
	}
	return proxy, nil
}

// NewLocalClient takes every dependency as a parameter rather than
// constructing any of them itself: proxy is shared with the
// LocalServerFactory main builds around it, dialers/servers/yamuxConfigs are
// factories (only ever constructed in main), and backoff has no reason to
// exist before main wires up the rest.
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

func (c *LocalClient) Run(ctx context.Context) error {
	b := c.backoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess, err := DialTunnel(ctx, c.opts.RemoteURL, c.dialers, c.yamuxConfigs)
		if err != nil {
			c.log.Printf("tunnel dial failed: %v", err)
			if !sleepCtx(ctx, b.Next()) {
				return ctx.Err()
			}
			continue
		}
		c.log.Printf("tunnel connected to %s", c.opts.RemoteURL)
		b.Reset()

		srv := c.servers()
		cancelled, serveErr := runTunnelSession(ctx, sess, func() error { return srv.Serve(sess) })
		if cancelled {
			return ctx.Err()
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			c.log.Printf("tunnel session ended: %v", serveErr)
		}

		if !sleepCtx(ctx, b.Next()) {
			return ctx.Err()
		}
	}
}

// waitOrCancel waits for either ctx to be cancelled or done to fire. On
// cancellation it calls onCancel (unblocking whatever done's producer is
// waiting on) and then waits for done itself, so the producer's goroutine
// never leaks; it reports true in that case. Otherwise it returns false
// once done fires on its own.
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
