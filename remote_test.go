// Unit tests for NewRemoteHandler's routing and authorization branches,
// driven without the live TLS/yamux tunnel integration_test.go sets up.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestRemoteHandler(t *testing.T) (http.Handler, *SessionRegistry) {
	t.Helper()
	cfg := DefaultConfig()
	reg := NewSessionRegistry()
	proxy := NewRemoteProxy(reg, log.Default())
	tunnelFactory := NewTunnelFactory(NewYamuxConfig(cfg.KeepAliveInterval, cfg.StreamOpenTimeout), context.Background())
	handler := NewRemoteHandler(context.Background(), proxy, reg, tunnelFactory, cfg.TunnelPath, log.Default())
	return handler, reg
}

func TestRemoteHandlerRejectsWrongOU(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	deviceLeaf := ca.issueDevice(t, "phone")
	tunnelLeaf := ca.issueTunnel(t, "home")

	tests := []struct {
		name string
		leaf *testLeaf
		path string
	}{
		{"device cert on tunnel path", deviceLeaf, DefaultConfig().TunnelPath},
		{"tunnel cert on ordinary path", tunnelLeaf, "/anything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _ := newTestRemoteHandler(t)
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.TLS = stateFor(t, tt.leaf)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestRemoteHandlerRejectsNoClientCert(t *testing.T) {
	handler, _ := newTestRemoteHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRemoteHandlerDeviceRequestNoTunnelReturns503(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	deviceLeaf := ca.issueDevice(t, "phone")
	handler, _ := newTestRemoteHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.TLS = stateFor(t, deviceLeaf)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestRemoteServerListenAndServePropagatesRealError exercises the branch that
// distinguishes a genuine ListenAndServeTLS failure from the expected
// ErrServerClosed on shutdown: every other test only ever sees the "runs
// fine" or "shut down via ctx cancel" outcomes, never a real error.
func TestRemoteServerListenAndServePropagatesRealError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String() // held open, so binding it again below fails

	srv := NewRemoteServer(addr, &tls.Config{}, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = srv.ListenAndServe(ctx)
	if err == nil {
		t.Fatal("ListenAndServe() error = nil, want an error for an address already in use")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe() = %v, want a real error, not ErrServerClosed", err)
	}
}

// TestRemoteServerListenAndServeReturnsNilOnGracefulShutdown exercises the
// other side of the err != nil && !errors.Is(err, ErrServerClosed) branch
// from the real-error test above: here ListenAndServeTLS's returned error
// actually is ErrServerClosed, the one case that tells && and || apart
// (both operands true, as in a real bind error, gives the same result under
// either operator). Nothing previously checked ListenAndServe's return value
// on the graceful-shutdown path at all.
func TestRemoteServerListenAndServeReturnsNilOnGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	serverLeaf, err := ca.issue(testLeafOptions{
		CommonName: "127.0.0.1", OU: "server", DNSNames: []string{"127.0.0.1"}, IsServer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	certPath := writePEM(t, dir, "server.crt", serverLeaf.CertPEM)
	keyPath := writePEM(t, dir, "server.key", serverLeaf.KeyPEM)
	tlsConf, err := NewServerTLSConfig(CertPaths{CA: caPath, Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := NewRemoteServer(addr, tlsConf, http.NotFoundHandler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	waitForListener(t, addr)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe() = %v, want nil on graceful shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe() did not return after ctx cancellation")
	}
}

func TestRemoteHandlerFailedTunnelAcceptLeavesRegistryEmpty(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	tunnelLeaf := ca.issueTunnel(t, "home")
	handler, reg := newTestRemoteHandler(t)
	// Not a real websocket upgrade, so AcceptTunnel fails fast — which is
	// what exercises acceptTunnel's error path.
	req := httptest.NewRequest(http.MethodGet, DefaultConfig().TunnelPath, nil)
	req.TLS = stateFor(t, tunnelLeaf)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := reg.Get(); got != nil {
		t.Fatalf("registry should stay empty after a failed tunnel accept, got %v", got)
	}
}
