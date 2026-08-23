// The remote half of the proxy: a public mTLS listener that accepts the
// home tunnel connection on TunnelPath and forwards every other request
// through it to opencode running locally.
package main

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
)

const RemoteVersionHeader = "X-Opencode-Proxy-Remote-Version"

type RemoteOptions struct {
	Addr   string
	TLS    *tls.Config
	Logger *log.Logger
}

type RemoteServer struct {
	opts RemoteOptions
	reg  sessionRegistry
	log  *log.Logger

	proxy *httputil.ReverseProxy
	srv   *http.Server
}

func NewRemoteServer(opts RemoteOptions) *RemoteServer {
	l := opts.Logger
	if l == nil {
		l = log.Default()
	}
	s := &RemoteServer{opts: opts, log: l}
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
	s.srv = &http.Server{
		Addr:      opts.Addr,
		TLSConfig: opts.TLS,
		Handler:   remoteWithVersionHeader(http.HandlerFunc(s.handle)),
	}
	return s
}

func (s *RemoteServer) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
	}()
	// Certificates are already embedded in TLSConfig, so no cert/key file args.
	if err := s.srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Pre-setting the header (rather than in ReverseProxy's ModifyResponse) is
// safe and covers every response path uniformly, including 403/503:
// httputil.ReverseProxy only adds backend headers via copyHeader, it never
// clears what's already on the ResponseWriter.
func remoteWithVersionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(RemoteVersionHeader, Version)
		next.ServeHTTP(w, r)
	})
}

func (s *RemoteServer) handle(w http.ResponseWriter, r *http.Request) {
	state := r.TLS

	if r.URL.Path == TunnelPath {
		if err := RequireOU(state, OUTunnel); err != nil {
			s.log.Printf("rejected tunnel upgrade from %s: %v", PeerName(state), err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.acceptTunnel(w, r)
		return
	}

	if err := RequireOU(state, OUDevice); err != nil {
		s.log.Printf("rejected request from %s: %v", PeerName(state), err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *RemoteServer) acceptTunnel(w http.ResponseWriter, r *http.Request) {
	sess, err := AcceptTunnel(w, r)
	if err != nil {
		s.log.Printf("tunnel accept failed: %v", err)
		return
	}
	s.log.Printf("tunnel connected from %s", PeerName(r.TLS))
	s.reg.set(sess)
	<-sess.CloseChan()
	s.reg.clear(sess)
	s.log.Printf("tunnel disconnected")
}
