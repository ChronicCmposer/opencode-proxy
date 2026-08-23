package main

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func stateFor(t *testing.T, leaf *testLeaf) *tls.ConnectionState {
	t.Helper()
	tc, err := leaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(tc.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
}

func TestRequireOU(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	tunnelLeaf, err := ca.issue(testLeafOptions{CommonName: "home-mac", OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	deviceLeaf, err := ca.issue(testLeafOptions{CommonName: "phone", OU: OUDevice})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := ca.issue(testLeafOptions{CommonName: "nobody", OU: "something-else"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		leaf    *testLeaf
		wantOU  string
		wantErr bool
	}{
		{"tunnel cert on tunnel endpoint", tunnelLeaf, OUTunnel, false},
		{"device cert on tunnel endpoint", deviceLeaf, OUTunnel, true},
		{"device cert on device endpoint", deviceLeaf, OUDevice, false},
		{"tunnel cert on device endpoint", tunnelLeaf, OUDevice, true},
		{"unrelated OU rejected everywhere", unrelated, OUDevice, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := stateFor(t, tt.leaf)
			err := RequireOU(state, tt.wantOU)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequireOU() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequireOUNilState(t *testing.T) {
	if err := RequireOU(nil, OUDevice); err == nil {
		t.Fatal("expected error for nil connection state")
	}
}
