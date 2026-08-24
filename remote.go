// The remote half of the proxy: a public mTLS listener that accepts the
// home tunnel connection on TunnelPath and forwards every other request
// through it to opencode running locally.
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
	"time"
)

const RemoteVersionHeader = "X-Opencode-Proxy-Remote-Version"

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
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       0,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			l.Printf("proxy error for %s: %v", r.URL.Path, err)
			http.Error(w, "no tunnel connected", http.StatusServiceUnavailable)
		},
	}
}

// NewRemoteHandler is the top-level request handler: it splits tunnel
// upgrades on TunnelPath from ordinary device requests, enforcing each
// side's required client-certificate role before dispatch. ctx governs the
// server's lifetime: acceptTunnel closes an active tunnel session when ctx
// is cancelled, mirroring LocalClient.Run's shutdown handling on the other
// end — without it, a hijacked tunnel connection is invisible to
// http.Server.Shutdown and would only end when the process itself exits.
func NewRemoteHandler(ctx context.Context, proxy *httputil.ReverseProxy, reg *SessionRegistry, yamuxConfigs YamuxConfigFactory, l *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := r.TLS

		if r.URL.Path == TunnelPath {
			if err := RequireOU(state, OUTunnel); err != nil {
				l.Printf("rejected tunnel upgrade from %s: %v", PeerName(state), err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			acceptTunnel(ctx, w, r, reg, yamuxConfigs, l)
			return
		}

		if err := RequireOU(state, OUDevice); err != nil {
			l.Printf("rejected request from %s: %v", PeerName(state), err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func acceptTunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, reg *SessionRegistry, yamuxConfigs YamuxConfigFactory, l *log.Logger) {
	sess, err := AcceptTunnel(w, r, yamuxConfigs)
	if err != nil {
		l.Printf("tunnel accept failed: %v", err)
		return
	}
	l.Printf("tunnel connected from %s", PeerName(r.TLS))
	reg.Set(sess)
	if cancelled, _ := runTunnelSession(ctx, sess, nil); cancelled {
		l.Printf("tunnel closed on shutdown")
	} else {
		l.Printf("tunnel disconnected")
	}
	reg.Clear(sess)
}

// NewRemoteHTTPServer wraps handler with the addr/TLS the public listener
// serves on. Certificates are already embedded in tlsConf, so
// ListenAndServe passes no cert/key file args.
func NewRemoteHTTPServer(addr string, tlsConf *tls.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:      addr,
		TLSConfig: tlsConf,
		Handler:   handler,
	}
}

// RemoteServer runs the public mTLS listener main builds via
// NewRemoteHTTPServer.
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
