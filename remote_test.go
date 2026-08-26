// Unit tests for NewRemoteHandler's routing and authorization branches,
// driven without the live TLS/yamux tunnel integration_test.go sets up.
package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestRemoteHandler(t *testing.T) (http.Handler, *SessionRegistry) {
	t.Helper()
	cfg := DefaultConfig()
	reg := NewSessionRegistry()
	tunnelFactory := NewTunnelFactory(NewYamuxConfig(cfg.KeepAliveInterval, cfg.StreamOpenTimeout), context.Background())
	handler := NewRemoteReverseProxy(context.Background(), reg, tunnelFactory, cfg.TunnelPath, cfg.MaxConcurrentStreams, cfg.MaxRequestBytes, log.Default())
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
