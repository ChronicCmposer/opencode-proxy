// The remote half of the proxy: a public mTLS listener that accepts the
// home tunnel connection on its configured tunnel path and forwards every
// other request through it to opencode running locally.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/hashicorp/yamux"
)

const RemoteVersionHeader = "X-Opencode-Proxy-Remote-Version"

type SessionRegistry struct {
	mu   sync.Mutex
	sess *yamux.Session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{}
}

// Set installs sess as the current tunnel only if no live tunnel is already
// connected, reporting whether it was accepted. It is deliberately
// first-writer-wins rather than last-writer-wins: blindly replacing a healthy
// session would let anyone holding the tunnel certificate silently evict the
// real home tunnel and man-in-the-middle every device request through an
// attacker-controlled backend. A stale session left behind by a dropped
// connection is not "live" — yamux keepalive marks it closed (IsClosed),
// after which the next Set is accepted, so a genuine reconnect isn't blocked.
func (r *SessionRegistry) Set(sess *yamux.Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess != nil && !r.sess.IsClosed() {
		return false
	}
	r.sess = sess
	return true
}

// Get returns the current session, or nil if none is connected. The
// returned session can still be closed by a concurrent Set/Clear right
// after Get returns it — callers don't need to guard against that: yamux's
// Open (and CloseChan) on an already-closed session simply fails/fires
// immediately, which the reverse proxy's normal dial-error handling already
// covers.
func (r *SessionRegistry) Get() *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil || r.sess.IsClosed() {
		return nil
	}
	return r.sess
}

func (r *SessionRegistry) Clear(sess *yamux.Session) {
	r.mu.Lock()
	if r.sess == sess { // don't clobber a newer session Set() already installed
		r.sess = nil
	}
	r.mu.Unlock()
}

// RemoteProxyPolicy groups the per-request security and resource controls the
// remote reverse proxy enforces. They travel together the way CertPaths does:
// bundling them keeps NewRemoteReverseProxy a two-collaborator constructor
// instead of a growing list of positional knobs where a caller could transpose
// two same-typed arguments unnoticed.
type RemoteProxyPolicy struct {
	// TunnelPath is the one HTTP path treated as a tunnel upgrade; every other
	// path is a device request.
	TunnelPath string
	// MaxConcurrentStreams caps device requests in flight through the single
	// tunnel; the (MaxConcurrentStreams+1)th is refused with 503 rather than
	// queued.
	MaxConcurrentStreams int
	// MaxRequestBytes caps each device request body.
	MaxRequestBytes int64
	// TunnelCN, when non-empty, pins the tunnel endpoint's certificate Common
	// Name: an OU-tunnel cert whose CN differs is refused, closing the
	// tunnel-takeover vector where any holder of any tunnel-role cert could
	// seize the registry. Empty keeps the OU-only check (any tunnel-role cert
	// is accepted) for deployments that haven't enrolled a pinned identity.
	TunnelCN string
	// AllowedPathPrefixes, when non-empty, restricts device requests to paths
	// under one of these prefixes; every other path is refused with 403 before
	// it ever reaches the tunnel. Empty allows every path — the
	// backward-compatible default — but a deployment can narrow the blast
	// radius of a stolen device cert by listing only the paths it needs.
	AllowedPathPrefixes []string
}

// NewRemoteReverseProxy splits tunnel upgrades on policy.TunnelPath from
// ordinary device requests, enforcing each side's required client-certificate
// role (and, when configured, the pinned tunnel CN and device path allowlist)
// before dispatch; non-tunnel requests are forwarded through the reverse proxy
// it builds internally. ctx governs an accepted tunnel's lifetime: a hijacked
// connection is invisible to http.Server.Shutdown, so without it the session
// would only end when the process exits.
func NewRemoteReverseProxy(ctx context.Context, reg *SessionRegistry, tunnelFactory *TunnelFactory, policy RemoteProxyPolicy, l *log.Logger) http.Handler {
	// A buffered channel used as a counting semaphore: a device request must
	// take a slot before it opens a yamux stream, so no more than
	// MaxConcurrentStreams are ever in flight through the one tunnel. A full
	// channel means "saturated" — the request is turned away with 503 rather
	// than queued, so a flood fails fast instead of piling up goroutines.
	sem := make(chan struct{}, policy.MaxConcurrentStreams)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// opencode.tunnel is never resolved: DialContext below ignores
			// the address and opens a yamux stream instead. ReverseProxy
			// still needs *some* URL to rewrite the request against.
			pr.SetURL(&url.URL{Scheme: "http", Host: "opencode.tunnel"})
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			// sess.Open() takes no context of its own and blocks until a
			// stream opens or StreamOpenTimeout elapses; raceCtx lets a
			// cancelled request return immediately instead of waiting out
			// that (deliberately generous, see config.go) timeout.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				sess := reg.Get()
				if sess == nil {
					return nil, fmt.Errorf("no tunnel connected")
				}
				return raceCtx(ctx, sess.Open)
			},
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Printf("proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "no tunnel connected", http.StatusServiceUnavailable)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.TLS

		if r.URL.Path == policy.TunnelPath {
			if err := VerifyPeerRole(state, OUTunnel); err != nil {
				l.Printf("rejected tunnel upgrade from %s: %v", GetPeerSubjectCN(state), err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			// When a tunnel CN is pinned, role alone isn't enough: only the one
			// enrolled home endpoint may register as the tunnel, so a stolen or
			// duplicate tunnel-role cert can't take over the registry.
			if policy.TunnelCN != "" {
				if err := VerifyPeerCN(state, policy.TunnelCN); err != nil {
					l.Printf("rejected tunnel upgrade from %s: %v", GetPeerSubjectCN(state), err)
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			acceptTunnel(ctx, w, r, reg, tunnelFactory, l)
			return
		}

		if err := VerifyPeerRole(state, OUDevice); err != nil {
			l.Printf("rejected request from %s: %v", GetPeerSubjectCN(state), err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// A configured path allowlist bounds what a device cert can reach: a
		// request outside it is refused here, before a tunnel stream is ever
		// opened. An empty allowlist permits everything (the default).
		if !pathAllowed(policy.AllowedPathPrefixes, r.URL.Path) {
			l.Printf("rejected request from %s: path %q is not on the allowlist", GetPeerSubjectCN(state), r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			l.Printf("rejected request from %s: %d concurrent requests already in flight", GetPeerSubjectCN(state), policy.MaxConcurrentStreams)
			http.Error(w, "too many concurrent requests", http.StatusServiceUnavailable)
			return
		}

		// Cap the request body so a single device can't stream an unbounded
		// upload through the tunnel. Only the body is bounded; the SSE
		// response stream back the other way is untouched.
		r.Body = http.MaxBytesReader(w, r.Body, policy.MaxRequestBytes)
		proxy.ServeHTTP(w, r)
	})
}

// pathAllowed reports whether path is permitted by prefixes. An empty
// prefixes slice allows every path (allowlisting is opt-in). A path matches
// when it equals a prefix or sits beneath it — "/api" allows "/api" and
// "/api/foo" but not "/apiary", so a prefix names a path segment boundary
// rather than a raw string prefix.
func pathAllowed(prefixes []string, path string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func acceptTunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, reg *SessionRegistry, tunnelFactory *TunnelFactory, l *log.Logger) {
	sess, err := tunnelFactory.AcceptTunnel(w, r)
	if err != nil {
		l.Printf("tunnel accept failed: %v", err)
		return
	}
	if !reg.Set(sess) {
		// A healthy tunnel is already connected. Reject rather than replace:
		// see SessionRegistry.Set. This is either a duplicate/misconfigured
		// endpoint or an attempt to hijack the tunnel, so log it loudly.
		l.Printf("rejected tunnel from %s: a tunnel is already connected", GetPeerSubjectCN(r.TLS))
		sess.Close()
		return
	}
	l.Printf("tunnel connected from %s", GetPeerSubjectCN(r.TLS))
	defer reg.Clear(sess)
	if waitForTunnelClose(ctx, sess) {
		l.Printf("tunnel closed on shutdown")
	} else {
		l.Printf("tunnel disconnected")
	}
}
