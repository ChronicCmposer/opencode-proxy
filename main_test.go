package main

import (
	"strings"
	"testing"
)

// The remote half is default-deny: it must refuse to start unless both the
// tunnel identity is pinned and a device path allowlist is set, and accept a
// fully specified policy.
func TestRequireRemotePolicy(t *testing.T) {
	tests := []struct {
		name     string
		policy   RemoteProxyPolicy
		wantText string // "" means no error expected
	}{
		{
			name:     "missing both",
			policy:   RemoteProxyPolicy{},
			wantText: "tunnel-cn",
		},
		{
			name:     "missing allowlist only",
			policy:   RemoteProxyPolicy{TunnelCN: "home-mac"},
			wantText: "allowed-path-prefixes",
		},
		{
			name:     "missing tunnel CN only",
			policy:   RemoteProxyPolicy{AllowedPathPrefixes: []string{"/"}},
			wantText: "tunnel-cn",
		},
		{
			name:   "fully specified",
			policy: RemoteProxyPolicy{TunnelCN: "home-mac", AllowedPathPrefixes: []string{"/"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireRemotePolicy(tt.policy)
			if tt.wantText == "" {
				if err != nil {
					t.Fatalf("requireRemotePolicy() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("requireRemotePolicy() error = nil, want one mentioning %q", tt.wantText)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantText)
			}
		})
	}
}
