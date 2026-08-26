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
	return newTestRemoteHandlerWithPolicy(t, RemoteProxyPolicy{})
}

// newTestRemoteHandlerWithPolicy builds the remote handler, filling any policy
// field the caller left zero from DefaultConfig so a test only has to set the
// knob it's exercising.
func newTestRemoteHandlerWithPolicy(t *testing.T, policy RemoteProxyPolicy) (http.Handler, *SessionRegistry) {
	t.Helper()
	cfg := DefaultConfig()
	if policy.TunnelPath == "" {
		policy.TunnelPath = cfg.TunnelPath
	}
	if policy.MaxConcurrentStreams == 0 {
		policy.MaxConcurrentStreams = cfg.MaxConcurrentStreams
	}
	if policy.MaxRequestBytes == 0 {
		policy.MaxRequestBytes = cfg.MaxRequestBytes
	}
	reg := NewSessionRegistry()
	tunnelFactory := NewTunnelFactory(NewYamuxConfig(cfg.KeepAliveInterval, cfg.StreamOpenTimeout), context.Background())
	handler := NewRemoteReverseProxy(context.Background(), reg, tunnelFactory, nil, policy, log.Default())
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

// V2: with a pinned tunnel CN, a tunnel-role cert whose CN differs is refused
// at the upgrade before it can register a session, closing the takeover vector.
func TestRemoteHandlerPinnedTunnelCN(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	imposter := ca.issueTunnel(t, "attacker-mac")

	handler, reg := newTestRemoteHandlerWithPolicy(t, RemoteProxyPolicy{TunnelCN: "home-mac"})
	req := httptest.NewRequest(http.MethodGet, DefaultConfig().TunnelPath, nil)
	req.TLS = stateFor(t, imposter)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for wrong tunnel CN", rec.Code)
	}
	if got := reg.Get(); got != nil {
		t.Fatalf("registry should stay empty after a CN-rejected tunnel, got %v", got)
	}
}

// V5: a device request outside the configured path allowlist is refused with
// 403 before a tunnel stream is ever opened; an allowed path passes the check
// (and only then fails 503 for want of a tunnel).
func TestRemoteHandlerPathAllowlist(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	deviceLeaf := ca.issueDevice(t, "phone")

	tests := []struct {
		name string
		path string
		want int
	}{
		{"disallowed path refused", "/admin", http.StatusForbidden},
		{"allowed prefix passes role/path checks", "/api/session", http.StatusServiceUnavailable},
		{"exact prefix match passes", "/api", http.StatusServiceUnavailable},
		{"prefix is a segment boundary, not a string prefix", "/apiary", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _ := newTestRemoteHandlerWithPolicy(t, RemoteProxyPolicy{AllowedPathPrefixes: []string{"/api"}})
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.TLS = stateFor(t, deviceLeaf)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
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
