// Loopback integration test: browser -> remote proxy -> yamux tunnel ->
// local proxy -> opencode, all on 127.0.0.1 with an in-memory CA.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type harness struct {
	t          *testing.T
	ca         *testCA
	opencode   *httpServer
	remoteAddr string
	deviceCert tls.Certificate
	caPath     string
}

func newHarness(t *testing.T, opencodeHandler http.Handler) *harness {
	t.Helper()
	dir := t.TempDir()

	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	oc := startHTTPServer(t, opencodeHandler)

	serverLeaf, err := ca.issue(testLeafOptions{
		CommonName: "127.0.0.1", OU: "server", DNSNames: []string{"127.0.0.1"}, IsServer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := serverLeaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}

	tunnelLeaf, err := ca.issue(testLeafOptions{CommonName: "home-mac", OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	tunnelCert, err := tunnelLeaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}

	deviceLeaf, err := ca.issue(testLeafOptions{CommonName: "phone", OU: OUDevice})
	if err != nil {
		t.Fatal(err)
	}
	deviceCert, err := deviceLeaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}

	pool, err := LoadCAPool(caPath)
	if err != nil {
		t.Fatal(err)
	}

	remoteTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteAddr := ln.Addr().String()
	ln.Close() // RemoteServer binds its own listener via ListenAndServeTLS

	srv := NewRemoteServer(RemoteOptions{Addr: remoteAddr, TLS: remoteTLS})
	ctx, cancel := context.WithCancel(context.Background())
	go srv.ListenAndServe(ctx)
	t.Cleanup(cancel)
	waitForListener(t, remoteAddr)

	localTLS := &tls.Config{
		Certificates: []tls.Certificate{tunnelCert},
		RootCAs:      pool,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS12,
	}
	client, err := NewLocalClient(LocalOptions{
		RemoteURL:   "wss://" + remoteAddr + "/_tunnel",
		OpencodeURL: "http://" + oc.addr,
		TLS:         localTLS,
	}, NewTunnelDialerFactory(localTLS))
	if err != nil {
		t.Fatal(err)
	}
	client.SetServerFactory(NewLocalServerFactory(client.Handler()))
	lctx, lcancel := context.WithCancel(context.Background())
	t.Cleanup(lcancel)
	go client.Run(lctx)

	return &harness{
		t: t, ca: ca, opencode: oc, remoteAddr: remoteAddr,
		deviceCert: deviceCert, caPath: caPath,
	}
}

func (h *harness) deviceHTTPClient() *http.Client {
	pool, err := LoadCAPool(h.caPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{h.deviceCert},
				RootCAs:      pool,
				ServerName:   "127.0.0.1",
			},
		},
	}
}

func (h *harness) url(path string) string {
	return "https://" + h.remoteAddr + path
}

func waitForTunnel(t *testing.T, cl *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := cl.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("tunnel never became ready")
}

func TestRoundTripAndAuthorizationPassthrough(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	h := newHarness(t, mux)
	cl := h.deviceHTTPClient()
	waitForTunnel(t, cl, h.url("/echo"))

	req, _ := http.NewRequest(http.MethodPost, h.url("/echo"), strings.NewReader("hello opencode"))
	req.Header.Set("Authorization", "Basic b3BlbmNvZGU6c2VjcmV0")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello opencode" {
		t.Fatalf("body round-trip: got %q", body)
	}
	if got := gotAuth.Load().(string); got != "Basic b3BlbmNvZGU6c2VjcmV0" {
		t.Fatalf("Authorization header not preserved: got %q", got)
	}
	if got := resp.Header.Get(LocalVersionHeader); got == "" {
		t.Errorf("%s missing on proxied response", LocalVersionHeader)
	}
	if got := resp.Header.Get(RemoteVersionHeader); got == "" {
		t.Errorf("%s missing on proxied response", RemoteVersionHeader)
	}
}

func TestSSEIsStreamedIncrementally(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: tick-%d\n\n", i)
			flusher.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	h := newHarness(t, mux)
	cl := h.deviceHTTPClient()
	waitForTunnel(t, cl, h.url("/event"))

	resp, err := cl.Get(h.url("/event"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var lastArrival time.Time
	var gaps []time.Duration
	for i := 0; i < 3; i++ {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("reading event %d: %v", i, err)
			}
			if strings.HasPrefix(line, "data:") {
				now := time.Now()
				if !lastArrival.IsZero() {
					gaps = append(gaps, now.Sub(lastArrival))
				}
				lastArrival = now
				break
			}
		}
	}
	// If the proxy buffered the whole response instead of flushing each
	// write, all three events would arrive back-to-back with ~0 gap.
	for _, g := range gaps {
		if g < 100*time.Millisecond {
			t.Fatalf("events arrived without streaming delay (gap %v) - response was buffered", g)
		}
	}
}

func TestNoTunnelReturns503(t *testing.T) {
	dir := t.TempDir()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	os.WriteFile(caPath, ca.CertPEM, 0o600)

	serverLeaf, _ := ca.issue(testLeafOptions{CommonName: "127.0.0.1", OU: "server", DNSNames: []string{"127.0.0.1"}, IsServer: true})
	serverCert, _ := serverLeaf.tlsCert()
	deviceLeaf, _ := ca.issue(testLeafOptions{CommonName: "phone", OU: OUDevice})
	deviceCert, _ := deviceLeaf.tlsCert()
	pool, _ := LoadCAPool(caPath)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	srv := NewRemoteServer(RemoteOptions{Addr: addr, TLS: &tls.Config{
		Certificates: []tls.Certificate{serverCert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.ListenAndServe(ctx)
	waitForListener(t, addr)

	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{deviceCert}, RootCAs: pool, ServerName: "127.0.0.1",
	}}}
	resp, err := cl.Get("https://" + addr + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get(RemoteVersionHeader); got == "" {
		t.Errorf("%s missing on 503 response", RemoteVersionHeader)
	}
	if got := resp.Header.Get(LocalVersionHeader); got != "" {
		t.Errorf("%s unexpectedly present with no tunnel connected: %q", LocalVersionHeader, got)
	}
}

func TestNoClientCertRejected(t *testing.T) {
	h := newHarness(t, http.NewServeMux())
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
	}}}
	_, err := cl.Get(h.url("/anything"))
	if err == nil {
		t.Fatal("expected TLS handshake failure without a client cert")
	}
}

func TestCertFromDifferentCARejected(t *testing.T) {
	h := newHarness(t, http.NewServeMux())

	otherCA, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, err := otherCA.issue(testLeafOptions{CommonName: "impostor", OU: OUDevice})
	if err != nil {
		t.Fatal(err)
	}
	otherCert, err := otherLeaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := LoadCAPool(h.caPath)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{otherCert}, RootCAs: pool, ServerName: "127.0.0.1",
	}}}
	_, err = cl.Get(h.url("/anything"))
	if err == nil {
		t.Fatal("expected TLS handshake failure for a cert from an untrusted CA")
	}
}

type httpServer struct{ addr string }

func startHTTPServer(t *testing.T, h http.Handler) *httpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &httpServer{addr: ln.Addr().String()}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}
