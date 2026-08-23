// The local half of the proxy: it dials the remote tunnel outbound,
// reconnecting with backoff, and reverse-proxies every request that
// arrives over it to opencode's local HTTP server.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const LocalVersionHeader = "X-Opencode-Proxy-Local-Version"

type LocalOptions struct {
	RemoteURL   string
	OpencodeURL string
	TLS         *tls.Config
	Logger      *log.Logger
}

type LocalClient struct {
	opts    LocalOptions
	log     *log.Logger
	proxy   *httputil.ReverseProxy
	dialers *TunnelDialerFactory
	servers *LocalServerFactory
	backoff *Backoff
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

// NewLocalClient takes dialers rather than constructing it itself: dialers is
// a factory, and factories are only ever constructed in main.
func NewLocalClient(opts LocalOptions, dialers *TunnelDialerFactory) (*LocalClient, error) {
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	target, err := url.Parse(opts.OpencodeURL)
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
	return &LocalClient{
		opts:    opts,
		log:     l,
		proxy:   proxy,
		dialers: dialers,
		backoff: NewBackoff(),
	}, nil
}

// Handler returns the request handler main wraps in a LocalServerFactory;
// main is where that factory is constructed.
func (c *LocalClient) Handler() http.Handler {
	return localWithVersionHeader(c.proxy)
}

// SetServerFactory wires in the LocalServerFactory main constructed. Run
// cannot proceed until this has been called.
func (c *LocalClient) SetServerFactory(f *LocalServerFactory) {
	c.servers = f
}

func (c *LocalClient) Run(ctx context.Context) error {
	b := c.backoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess, err := DialTunnel(ctx, c.opts.RemoteURL, c.dialers)
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

// See remoteWithVersionHeader for why pre-setting the header is safe with
// httputil.ReverseProxy.
func localWithVersionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(LocalVersionHeader, Version)
		next.ServeHTTP(w, r)
	})
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
