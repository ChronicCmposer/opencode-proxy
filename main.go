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
		isLocal  = flag.Bool("local", false, "run the Mac-side tunnel client")
		isRemote = flag.Bool("remote", false, "run the AWS-side tunnel server")

		caPath   = flag.String("ca", "", "path to the private CA bundle (PEM)")
		certPath = flag.String("cert", "", "path to this endpoint's certificate (PEM)")
		keyPath  = flag.String("key", "", "path to this endpoint's private key (PEM)")

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *isLocal {
		return runLocal(ctx, *caPath, *certPath, *keyPath, *remoteURL, *opencodeURL, *serverName)
	} else {
		return runRemote(ctx, *caPath, *certPath, *keyPath, *addr)
	}
}

func runLocal(ctx context.Context, caPath, certPath, keyPath, remoteURL, opencodeURL, serverName string) error {
	if remoteURL == "" {
		return fmt.Errorf("--remote-url is required with --local")
	}
	tlsConf, err := ClientConfig(caPath, certPath, keyPath, serverName)
	if err != nil {
		return err
	}
	client, err := NewLocalClient(LocalOptions{
		RemoteURL:   remoteURL,
		OpencodeURL: opencodeURL,
		TLS:         tlsConf,
	})
	if err != nil {
		return err
	}
	return client.Run(ctx)
}

func runRemote(ctx context.Context, caPath, certPath, keyPath, addr string) error {
	tlsConf, err := ServerConfig(caPath, certPath, keyPath)
	if err != nil {
		return err
	}
	srv := NewRemoteServer(RemoteOptions{
		Addr:   addr,
		TLS:    tlsConf,
		Logger: log.Default(),
	})
	return srv.ListenAndServe(ctx)
}
