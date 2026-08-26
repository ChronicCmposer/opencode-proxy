// Command opencode-proxy runs either half of a secure reverse tunnel that
// exposes a home opencode server to the internet without port-forwarding.
//
//	opencode-proxy --local  --remote-url wss://code.example.com/_tunnel \
//	                --ca ca.crt --cert tunnel.crt --key tunnel.key
//	opencode-proxy --remote --addr :443 \
//	                --ca ca.crt --cert server.crt --key server.key
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "opencode-proxy:", err)
		os.Exit(1)
	}
}

// flags holds the parsed and validated CLI flags, so run() can go straight
// from parsing to wiring collaborators.
type flags struct {
	isLocal bool

	caPath, certPath, keyPath string
	configPath                string
	revokedPath               string

	remoteURL, opencodeURL, serverName string

	addr     string
	tunnelCN string
}

func parseFlags() (*flags, error) {
	isLocal := flag.Bool("local", false, "run the local tunnel client")
	isRemote := flag.Bool("remote", false, "run the remote tunnel server")

	caPath := flag.String("ca", "", "path to the private CA bundle (PEM)")
	certPath := flag.String("cert", "", "path to this endpoint's certificate (PEM)")
	keyPath := flag.String("key", "", "path to this endpoint's private key (PEM)")

	configPath := flag.String("config", "", "path to the JSON config file (optional; built-in defaults are used when omitted)")

	revokedPath := flag.String("revoked", "", "--remote: path to a certificate revocation list (one hex serial per line; optional)")

	remoteURL := flag.String("remote-url", "", "--local: wss:// URL of the remote proxy's tunnel endpoint")
	opencodeURL := flag.String("opencode-url", "http://127.0.0.1:4096", "--local: URL of the local opencode server")
	serverName := flag.String("server-name", "", "--local: TLS server name to verify on the remote (defaults to the host in --remote-url)")

	addr := flag.String("addr", ":443", "--remote: address to listen on")
	tunnelCN := flag.String("tunnel-cn", "", "--remote: if set, pin the tunnel endpoint certificate's Common Name; only this identity may register the tunnel (overrides config tunnel-cn)")

	flag.Parse()

	if *isLocal == *isRemote {
		return nil, fmt.Errorf("exactly one of --local or --remote is required")
	}
	if *caPath == "" || *certPath == "" || *keyPath == "" {
		return nil, fmt.Errorf("--ca, --cert, and --key are required")
	}
	if *isLocal && *remoteURL == "" {
		return nil, fmt.Errorf("--remote-url is required with --local")
	}

	return &flags{
		isLocal:     *isLocal,
		caPath:      *caPath,
		certPath:    *certPath,
		keyPath:     *keyPath,
		configPath:  *configPath,
		revokedPath: *revokedPath,
		remoteURL:   *remoteURL,
		opencodeURL: *opencodeURL,
		serverName:  *serverName,
		addr:        *addr,
		tunnelCN:    *tunnelCN,
	}, nil
}

func run() error {
	f, err := parseFlags()
	if err != nil {
		return err
	}

	cfg, err := LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	certs := CertPaths{CA: f.caPath, Cert: f.certPath, Key: f.keyPath}
	yamuxConfig := NewYamuxConfig(cfg.KeepAliveInterval, cfg.StreamOpenTimeout)
	netConnCtx := context.Background()
	tunnelFactory := NewTunnelFactory(yamuxConfig, netConnCtx)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.Default()

	if f.isLocal {
		proxy, err := NewLocalReverseProxy(f.opencodeURL, logger)
		if err != nil {
			return err
		}
		backoff := NewBackoff(cfg.BackoffMin, cfg.BackoffMax)
		tlsConf, err := NewClientTLSConfig(certs, f.serverName)
		if err != nil {
			return err
		}
		dialer := NewTunnelDialer(tlsConf)
		handler := WithVersionHeader(LocalVersionHeader, Version, proxy)
		// server is safe to share across every reconnect: net/http only
		// leaves a Server unusable for a future Serve call once Shutdown or
		// Close has actually been invoked on it (permanently, via an
		// internal flag nothing else ever sets). Run never calls either —
		// it ends each session by closing the yamux session (this Serve
		// call's Listener), which just makes Serve return; the Server
		// itself is untouched.
		//
		// ReadHeaderTimeout guards against a slow-header (Slowloris) peer
		// tying up a goroutine indefinitely. No ReadTimeout/WriteTimeout:
		// this side proxies opencode's own responses, including the
		// long-lived GET /event SSE stream, so a whole-request deadline
		// would cut those off.
		server := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			MaxHeaderBytes:    cfg.MaxHeaderBytes,
		}
		client := NewLocalClient(f.remoteURL, logger, dialer, server, tunnelFactory, backoff)
		return client.Run(ctx)
	}

	reg := NewSessionRegistry()
	revocation, err := NewRevocationList(f.revokedPath)
	if err != nil {
		return err
	}
	tlsConf, err := NewServerTLSConfig(certs, revocation)
	if err != nil {
		return err
	}
	// The --tunnel-cn flag, when given, overrides the config file's value: an
	// operator pinning the tunnel identity at the command line shouldn't have
	// to also edit the config.
	tunnelCN := cfg.TunnelCN
	if f.tunnelCN != "" {
		tunnelCN = f.tunnelCN
	}
	policy := RemoteProxyPolicy{
		TunnelPath:           cfg.TunnelPath,
		MaxConcurrentStreams: cfg.MaxConcurrentStreams,
		MaxRequestBytes:      cfg.MaxRequestBytes,
		TunnelCN:             tunnelCN,
		AllowedPathPrefixes:  cfg.AllowedPathPrefixes,
	}
	handler := NewRemoteReverseProxy(ctx, reg, tunnelFactory, policy, logger)
	handler = WithVersionHeader(RemoteVersionHeader, Version, handler)
	// ReadHeaderTimeout bounds the slow-header (Slowloris) window on a
	// listener that faces the public internet. No ReadTimeout/WriteTimeout:
	// device requests stream opencode's responses (notably the long-lived
	// GET /event SSE stream) back through the tunnel, and a whole-request
	// deadline would sever those.
	srv := &http.Server{
		Addr:              f.addr,
		TLSConfig:         tlsConf,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// WithVersionHeader sets header to version on every response. Pre-setting it
// (rather than in httputil.ReverseProxy's ModifyResponse) covers every
// response path uniformly, including 403/503 error paths: ReverseProxy only
// adds backend headers via copyHeader, it never clears what's already on the
// ResponseWriter.
func WithVersionHeader(header, version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header, version)
		next.ServeHTTP(w, r)
	})
}
