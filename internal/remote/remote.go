// Package remote implements the AWS-side half of the proxy: a public mTLS
// listener that accepts the home tunnel connection on Path and forwards
// every other request through it to opencode running on the Mac.
package remote

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/ChronicCmposer/opencode-proxy/internal/tlsconf"
	"github.com/ChronicCmposer/opencode-proxy/internal/tunnel"
	"github.com/ChronicCmposer/opencode-proxy/internal/version"
)

const VersionHeader = "X-Opencode-Proxy-Remote-Version"

type Options struct {
	Addr   string
	TLS    *tls.Config
	Logger *log.Logger
}

type Server struct {
	opts Options
	reg  sessionRegistry
	log  *log.Logger

	proxy *httputil.ReverseProxy
}

func New(opts Options) *Server {
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	s := &Server{opts: opts, log: l}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// opencode.tunnel is never resolved: DialContext below ignores
			// the address and opens a yamux stream instead. ReverseProxy
			// still needs *some* URL to rewrite the request against.
			pr.SetURL(&url.URL{Scheme: "http", Host: "opencode.tunnel"})
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				sess := s.reg.get()
				if sess == nil {
					return nil, fmt.Errorf("no tunnel connected")
				}
				return sess.Open()
			},
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       0,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Printf("proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "no tunnel connected", http.StatusServiceUnavailable)
		},
	}
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:      s.opts.Addr,
		TLSConfig: s.opts.TLS,
		Handler:   withVersionHeader(http.HandlerFunc(s.handle)),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	// Certificates are already embedded in TLSConfig, so no cert/key file args.
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Pre-setting the header (rather than in ReverseProxy's ModifyResponse) is
// safe and covers every response path uniformly, including 403/503:
// httputil.ReverseProxy only adds backend headers via copyHeader, it never
// clears what's already on the ResponseWriter.
func withVersionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, version.Version)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	state := r.TLS

	if r.URL.Path == tunnel.Path {
		if err := tlsconf.RequireOU(state, tlsconf.OUTunnel); err != nil {
			s.log.Printf("rejected tunnel upgrade from %s: %v", tlsconf.PeerName(state), err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.acceptTunnel(w, r)
		return
	}

	if err := tlsconf.RequireOU(state, tlsconf.OUDevice); err != nil {
		s.log.Printf("rejected request from %s: %v", tlsconf.PeerName(state), err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) acceptTunnel(w http.ResponseWriter, r *http.Request) {
	sess, err := tunnel.Accept(w, r)
	if err != nil {
		s.log.Printf("tunnel accept failed: %v", err)
		return
	}
	s.log.Printf("tunnel connected from %s", tlsconf.PeerName(r.TLS))
	s.reg.set(sess)
	<-sess.CloseChan()
	s.reg.clear(sess)
	s.log.Printf("tunnel disconnected")
}
