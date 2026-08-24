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
// WithVersionHeader, and hand the result to NewLocalServerFactory —
// letting every dependency reach NewLocalClient through its constructor
// instead of being patched in afterward.
func NewLocalProxy(opencodeURL string, l *log.Logger) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(opencodeURL)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		// Rewrite rather than the legacy Director, matching NewRemoteProxy:
		// Director appends to whatever X-Forwarded-For the caller supplied,
		// while Rewrite strips the client's forwarding headers first.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// opencode is addressed by its own host rather than the public
			// name the browser used; SetURL clears Out.Host, so set it back.
			pr.Out.Host = target.Host
		},
		// An explicit zero-valued Transport rather than DefaultTransport: no
		// env-var proxying and no idle-connection timeout on a link that only
		// ever reaches the local opencode server.
		Transport:     &http.Transport{},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Printf("opencode proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "opencode unreachable", http.StatusBadGateway)
		},
	}, nil
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

// Run dials the tunnel and serves it until ctx is cancelled, reconnecting
// with backoff in between. Cancellation is the expected way to stop, so Run
// returns nil for it rather than ctx.Err() — mirroring
// RemoteServer.ListenAndServe swallowing http.ErrServerClosed, so both
// halves exit 0 on a clean SIGTERM instead of disagreeing about it.
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

// waitToRetry sleeps for the next backoff interval, reporting false if ctx
// is cancelled first.
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
