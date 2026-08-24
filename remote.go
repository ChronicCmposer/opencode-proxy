// The remote half of the proxy: a public mTLS listener that accepts the
// home tunnel connection on its configured tunnel path and forwards every
// other request through it to opencode running locally.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

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

func (r *SessionRegistry) Set(sess *yamux.Session) {
	r.mu.Lock()
	old := r.sess
	r.sess = sess
	r.mu.Unlock()
	if old != nil {
		old.Close()
	}
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

// NewRemoteProxy builds the reverse proxy that forwards device requests
// through whatever tunnel session reg currently holds.
func NewRemoteProxy(reg *SessionRegistry, l *log.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// opencode.tunnel is never resolved: DialContext below ignores
			// the address and opens a yamux stream instead. ReverseProxy
			// still needs *some* URL to rewrite the request against.
			pr.SetURL(&url.URL{Scheme: "http", Host: "opencode.tunnel"})
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				sess := reg.Get()
				if sess == nil {
					return nil, fmt.Errorf("no tunnel connected")
				}
				return sess.Open()
			},
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Printf("proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "no tunnel connected", http.StatusServiceUnavailable)
		},
	}
}

// NewRemoteHandler splits tunnel upgrades on tunnelPath from ordinary
// device requests, enforcing each side's required client-certificate role
// before dispatch. ctx governs an accepted tunnel's lifetime: a hijacked
// connection is invisible to http.Server.Shutdown, so without it the
// session would only end when the process exits.
func NewRemoteHandler(ctx context.Context, proxy *httputil.ReverseProxy, reg *SessionRegistry, tunnelFactory *TunnelFactory, tunnelPath string, l *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.TLS

		if r.URL.Path == tunnelPath {
			if err := VerifyPeerRole(state, OUTunnel); err != nil {
				l.Printf("rejected tunnel upgrade from %s: %v", GetPeerSubjectCN(state), err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			acceptTunnel(ctx, w, r, reg, tunnelFactory, l)
			return
		}

		if err := VerifyPeerRole(state, OUDevice); err != nil {
			l.Printf("rejected request from %s: %v", GetPeerSubjectCN(state), err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func acceptTunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, reg *SessionRegistry, tunnelFactory *TunnelFactory, l *log.Logger) {
	sess, err := tunnelFactory.AcceptTunnel(w, r)
	if err != nil {
		l.Printf("tunnel accept failed: %v", err)
		return
	}
	l.Printf("tunnel connected from %s", GetPeerSubjectCN(r.TLS))
	reg.Set(sess)
	defer reg.Clear(sess)
	if waitForTunnelClose(ctx, sess) {
		l.Printf("tunnel closed on shutdown")
	} else {
		l.Printf("tunnel disconnected")
	}
}

func NewRemoteHTTPServer(addr string, tlsConf *tls.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:      addr,
		TLSConfig: tlsConf,
		Handler:   handler,
	}
}

type RemoteServer struct {
	srv *http.Server
}

func NewRemoteServer(srv *http.Server) *RemoteServer {
	return &RemoteServer{srv: srv}
}

func (s *RemoteServer) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
	}()
	if err := s.srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
