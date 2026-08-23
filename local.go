// The Mac-side half of the proxy: it dials the remote tunnel outbound,
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
	opts  LocalOptions
	log   *log.Logger
	proxy *httputil.ReverseProxy
}

func NewLocalClient(opts LocalOptions) (*LocalClient, error) {
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
	return &LocalClient{opts: opts, log: l, proxy: proxy}, nil
}

func (c *LocalClient) Run(ctx context.Context) error {
	b := NewBackoff()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess, err := DialTunnel(ctx, c.opts.RemoteURL, c.opts.TLS)
		if err != nil {
			c.log.Printf("tunnel dial failed: %v", err)
			if !sleepCtx(ctx, b.Next()) {
				return ctx.Err()
			}
			continue
		}
		c.log.Printf("tunnel connected to %s", c.opts.RemoteURL)
		b.Reset()

		srv := &http.Server{Handler: localWithVersionHeader(c.proxy)}
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
