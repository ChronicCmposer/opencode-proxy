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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

	remoteURL, opencodeURL, serverName string

	addr string
}

func parseFlags() (*flags, error) {
	isLocal := flag.Bool("local", false, "run the local tunnel client")
	isRemote := flag.Bool("remote", false, "run the remote tunnel server")

	caPath := flag.String("ca", "", "path to the private CA bundle (PEM)")
	certPath := flag.String("cert", "", "path to this endpoint's certificate (PEM)")
	keyPath := flag.String("key", "", "path to this endpoint's private key (PEM)")

	configPath := flag.String("config", "", "path to the JSON config file (optional; built-in defaults are used when omitted)")

	remoteURL := flag.String("remote-url", "", "--local: wss:// URL of the remote proxy's tunnel endpoint")
	opencodeURL := flag.String("opencode-url", "http://127.0.0.1:4096", "--local: URL of the local opencode server")
	serverName := flag.String("server-name", "", "--local: TLS server name to verify on the remote (defaults to the host in --remote-url)")

	addr := flag.String("addr", ":443", "--remote: address to listen on")

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
		remoteURL:   *remoteURL,
		opencodeURL: *opencodeURL,
		serverName:  *serverName,
		addr:        *addr,
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
		server := NewLocalServer(handler)
		client := NewLocalClient(f.remoteURL, logger, dialer, server, tunnelFactory, backoff)
		return client.Run(ctx)
	}

	reg := NewSessionRegistry()
	tlsConf, err := NewServerTLSConfig(certs)
	if err != nil {
		return err
	}
	handler := NewRemoteReverseProxy(ctx, reg, tunnelFactory, cfg.TunnelPath, logger)
	handler = WithVersionHeader(RemoteVersionHeader, Version, handler)
	srv := NewRemoteServer(f.addr, tlsConf, handler)
	return srv.ListenAndServe(ctx)
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
