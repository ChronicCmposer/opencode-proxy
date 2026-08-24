// The local half of the proxy: it dials the remote tunnel outbound,
// reconnecting with backoff, and reverse-proxies every request that
// arrives over it to opencode's local HTTP server.
package main

import (
	"context"
	"errors"
	"log"
	"math/rand"
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
	opts          LocalOptions
	log           *log.Logger
	proxy         *httputil.ReverseProxy
	dialer        *http.Client
	server        *http.Server
	tunnelFactory *TunnelFactory
	backoff       *Backoff
}

// NewLocalServer builds the http.Server LocalClient.Run serves each
// tunnel session over.
//
// A single instance is safe to share across every reconnect: net/http
// only leaves a Server unusable for a future Serve call once Shutdown or
// Close has actually been invoked on it (permanently, via an internal
// flag nothing else ever sets). Run never calls either — it ends each
// session by closing the yamux session (this Serve call's Listener),
// which just makes Serve return; the Server itself is untouched.
func NewLocalServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler}
}

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

func NewLocalClient(opts LocalOptions, proxy *httputil.ReverseProxy, dialer *http.Client, server *http.Server, tunnelFactory *TunnelFactory, backoff *Backoff) *LocalClient {
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	return &LocalClient{
		opts:          opts,
		log:           l,
		proxy:         proxy,
		dialer:        dialer,
		server:        server,
		tunnelFactory: tunnelFactory,
		backoff:       backoff,
	}
}

// Run dials the tunnel and serves it until ctx is cancelled, reconnecting
// with backoff in between. Cancellation is the expected way to stop, so it
// returns nil rather than ctx.Err(), leaving the process exit code 0 on a
// clean SIGTERM.
func (c *LocalClient) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		sess, err := c.tunnelFactory.DialTunnel(ctx, c.opts.RemoteURL, c.dialer)
		if err != nil {
			c.log.Printf("tunnel dial failed: %v", err)
			if !c.waitToRetry(ctx) {
				break
			}
			continue
		}
		c.log.Printf("tunnel connected to %s", c.opts.RemoteURL)
		c.backoff.Reset()

		cancelled, serveErr := runTunnelSession(ctx, sess, func() error { return c.server.Serve(sess) })
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

type Backoff struct {
	min, max time.Duration
	attempt  int
}

func NewBackoff(min, max time.Duration) *Backoff {
	return &Backoff{min: min, max: max}
}

// Next never returns more than b.max: jitter is added within the cap rather
// than on top of it, so callers can rely on b.max as a true upper bound.
func (b *Backoff) Next() time.Duration {
	base := b.nextBase()
	// rand.Int63n panics on n<=0, and base/5 underflows to 0 for sub-5ns
	// bases: skip jitter rather than risk it on an extreme configured value.
	jitterMax := int64(base) / 5
	if jitterMax <= 0 {
		return base
	}
	jitter := time.Duration(rand.Int63n(jitterMax))
	return min(base+jitter, b.max)
}

// nextBase returns the un-jittered delay for the current attempt, capped at
// b.max, and advances the attempt counter as long as it hasn't already
// reached the cap. Once min<<attempt reaches or overflows past b.max
// (overflow wraps a Duration's underlying int64 negative, so d<=0 also means
// "past max"), attempt stops advancing: shifting further would only ever
// yield b.max again, so there's no reason to keep counting.
func (b *Backoff) nextBase() time.Duration {
	d := b.min << b.attempt
	if d <= 0 || d > b.max {
		return b.max
	}
	b.attempt++
	return d
}

func (b *Backoff) Reset() {
	b.attempt = 0
}
