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
)

// Options configures a Server.
type Options struct {
	Addr   string // e.g. ":443"
	TLS    *tls.Config
	Logger *log.Logger
}

// Server is the remote proxy: it serves the public listener and holds the
// currently connected tunnel session, if any.
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
			// The destination host is irrelevant: DialContext always opens a
			// yamux stream instead of a real network connection. A stable
			// placeholder keeps ReverseProxy's URL rewriting happy.
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
			// Never buffer or time out a streaming response (opencode's
			// GET /event SSE stream in particular).
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       0,
		},
		FlushInterval: -1, // flush every write immediately, required for SSE
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Printf("proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "no tunnel connected", http.StatusServiceUnavailable)
		},
	}
	return s
}

// ListenAndServe runs the public mTLS listener until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:      s.opts.Addr,
		TLSConfig: s.opts.TLS,
		Handler:   http.HandlerFunc(s.handle),
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

	// Block until the session dies (Mac disconnects, network drop, etc.), then
	// deregister it so new requests get a clean 503 instead of hanging on a
	// dead session.
	<-sess.CloseChan()
	s.reg.clear(sess)
	s.log.Printf("tunnel disconnected")
}
