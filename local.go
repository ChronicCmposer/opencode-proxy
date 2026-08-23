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
	dialers      *TunnelDialerFactory
	servers      *LocalServerFactory
	yamuxConfigs *YamuxConfigFactory
	backoff      *Backoff
}

// LocalServerFactory builds a fresh http.Server for each reconnect: Run
// serves a new tunnel session every time, and http.Server must not be reused
// across sessions once it has been Serve'd and stopped.
type LocalServerFactory struct {
	handler http.Handler
}

func NewLocalServerFactory(handler http.Handler) *LocalServerFactory {
	return &LocalServerFactory{handler: handler}
}

func (f *LocalServerFactory) CreateServer() *http.Server {
	return &http.Server{Handler: f.handler}
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
func NewLocalClient(opts LocalOptions, proxy *httputil.ReverseProxy, dialers *TunnelDialerFactory, servers *LocalServerFactory, yamuxConfigs *YamuxConfigFactory, backoff *Backoff) *LocalClient {
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

		srv := c.servers.CreateServer()
		serveErr := make(chan error, 1)
		go func() { serveErr <- srv.Serve(sess) }()

		select {
		case <-ctx.Done():
			sess.Close()
			<-serveErr
			return ctx.Err()
		case err := <-serveErr:
			sess.Close()
			if !errors.Is(err, context.Canceled) {
				c.log.Printf("tunnel session ended: %v", err)
			}
		}

		if !sleepCtx(ctx, b.Next()) {
			return ctx.Err()
		}
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
