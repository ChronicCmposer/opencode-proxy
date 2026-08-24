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
	"net/http/httputil"
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

func run() error {
	var (
		isLocal  = flag.Bool("local", false, "run the local tunnel client")
		isRemote = flag.Bool("remote", false, "run the remote tunnel server")

		caPath   = flag.String("ca", "", "path to the private CA bundle (PEM)")
		certPath = flag.String("cert", "", "path to this endpoint's certificate (PEM)")
		keyPath  = flag.String("key", "", "path to this endpoint's private key (PEM)")

		configPath = flag.String("config", "", "path to the JSON config file (optional; built-in defaults are used when omitted)")

		remoteURL   = flag.String("remote-url", "", "--local: wss:// URL of the remote proxy's tunnel endpoint")
		opencodeURL = flag.String("opencode-url", "http://127.0.0.1:4096", "--local: URL of the local opencode server")
		serverName  = flag.String("server-name", "", "--local: TLS server name to verify on the remote (defaults to the host in --remote-url)")

		addr = flag.String("addr", ":443", "--remote: address to listen on")
	)
	flag.Parse()

	if *isLocal == *isRemote {
		return fmt.Errorf("exactly one of --local or --remote is required")
	}
	if *caPath == "" || *certPath == "" || *keyPath == "" {
		return fmt.Errorf("--ca, --cert, and --key are required")
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	certs := CertPaths{CA: *caPath, Cert: *certPath, Key: *keyPath}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := log.Default()

	if *isLocal {
		if *remoteURL == "" {
			return fmt.Errorf("--remote-url is required with --local")
		}
		proxy, err := NewLocalProxy(*opencodeURL, logger)
		if err != nil {
			return err
		}
		backoff := NewBackoff(cfg.BackoffMin, cfg.BackoffMax)
		return runLocal(ctx, cfg, certs, *remoteURL, *serverName, proxy, backoff, logger)
	}

	reg := NewSessionRegistry()
	proxy := NewRemoteProxy(reg, logger)
	return runRemote(ctx, cfg, certs, *addr, reg, proxy, logger)
}

func runLocal(ctx context.Context, cfg Config, certs CertPaths, remoteURL, serverName string, proxy *httputil.ReverseProxy, backoff *Backoff, logger *log.Logger) error {
	tlsConf, err := NewClientTLSConfig(certs, serverName)
	if err != nil {
		return err
	}
	dialerFactory := NewTunnelDialerFactory(tlsConf)
	serverFactory := NewLocalServerFactory(WithVersionHeader(LocalVersionHeader, Version, proxy))
	tunnelFactory := NewTunnelFactoryFromConfig(cfg)
	client := NewLocalClient(LocalOptions{
		RemoteURL: remoteURL,
		Logger:    logger,
	}, proxy, dialerFactory, serverFactory, tunnelFactory, backoff)
	return client.Run(ctx)
}

func runRemote(ctx context.Context, cfg Config, certs CertPaths, addr string, reg *SessionRegistry, proxy *httputil.ReverseProxy, logger *log.Logger) error {
	tlsConf, err := NewServerTLSConfig(certs)
	if err != nil {
		return err
	}
	tunnelFactory := NewTunnelFactoryFromConfig(cfg)
	handler := WithVersionHeader(RemoteVersionHeader, Version, NewRemoteHandler(ctx, proxy, reg, tunnelFactory, cfg.TunnelPath, logger))
	httpSrv := NewRemoteHTTPServer(addr, tlsConf, handler)
	srv := NewRemoteServer(httpSrv)
	return srv.ListenAndServe(ctx)
}
