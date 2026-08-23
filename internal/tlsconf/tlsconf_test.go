package tlsconf_test

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/ChronicCmposer/opencode-proxy/internal/testca"
	"github.com/ChronicCmposer/opencode-proxy/internal/tlsconf"
)

func stateFor(t *testing.T, leaf *testca.Leaf) *tls.ConnectionState {
	t.Helper()
	tc, err := leaf.TLSCert()
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
	ca, err := testca.New()
	if err != nil {
		t.Fatal(err)
	}
	tunnelLeaf, err := ca.Issue(testca.LeafOptions{CommonName: "home-mac", OU: tlsconf.OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	deviceLeaf, err := ca.Issue(testca.LeafOptions{CommonName: "phone", OU: tlsconf.OUDevice})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := ca.Issue(testca.LeafOptions{CommonName: "nobody", OU: "something-else"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		leaf    *testca.Leaf
		wantOU  string
		wantErr bool
	}{
		{"tunnel cert on tunnel endpoint", tunnelLeaf, tlsconf.OUTunnel, false},
		{"device cert on tunnel endpoint", deviceLeaf, tlsconf.OUTunnel, true},
		{"device cert on device endpoint", deviceLeaf, tlsconf.OUDevice, false},
		{"tunnel cert on device endpoint", tunnelLeaf, tlsconf.OUDevice, true},
		{"unrelated OU rejected everywhere", unrelated, tlsconf.OUDevice, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := stateFor(t, tt.leaf)
			err := tlsconf.RequireOU(state, tt.wantOU)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequireOU() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRequireOUNilState(t *testing.T) {
	if err := tlsconf.RequireOU(nil, tlsconf.OUDevice); err == nil {
		t.Fatal("expected error for nil connection state")
	}
}
