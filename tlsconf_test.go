package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
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

func TestVerifyPeerRole(t *testing.T) {
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
			err := VerifyPeerRole(state, tt.wantOU)
			if (err != nil) != tt.wantErr {
				t.Fatalf("VerifyPeerRole() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyPeerRoleNilState(t *testing.T) {
	if err := VerifyPeerRole(nil, OUDevice); err == nil {
		t.Fatal("expected error for nil connection state")
	}
}

func writePEM(t *testing.T, dir, name string, pem []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerAndClientConfig(t *testing.T) {
	dir := t.TempDir()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := writePEM(t, dir, "ca.crt", ca.CertPEM)

	serverLeaf, err := ca.issue(testLeafOptions{
		CommonName: "127.0.0.1", OU: "server", DNSNames: []string{"127.0.0.1"}, IsServer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverCertPath := writePEM(t, dir, "server.crt", serverLeaf.CertPEM)
	serverKeyPath := writePEM(t, dir, "server.key", serverLeaf.KeyPEM)

	serverConf, err := NewServerConfig(CertPaths{CA: caPath, Cert: serverCertPath, Key: serverKeyPath})
	if err != nil {
		t.Fatal(err)
	}
	if serverConf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", serverConf.ClientAuth)
	}
	if serverConf.ClientCAs == nil {
		t.Error("ClientCAs not set")
	}
	if len(serverConf.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1", len(serverConf.Certificates))
	}

	tunnelLeaf, err := ca.issue(testLeafOptions{CommonName: "home", OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	tunnelCertPath := writePEM(t, dir, "tunnel.crt", tunnelLeaf.CertPEM)
	tunnelKeyPath := writePEM(t, dir, "tunnel.key", tunnelLeaf.KeyPEM)

	clientConf, err := NewClientConfig(CertPaths{CA: caPath, Cert: tunnelCertPath, Key: tunnelKeyPath}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if clientConf.ServerName != "127.0.0.1" {
		t.Errorf("ServerName = %q, want 127.0.0.1", clientConf.ServerName)
	}
	if clientConf.RootCAs == nil {
		t.Error("RootCAs not set")
	}
	if len(clientConf.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1", len(clientConf.Certificates))
	}
}

func TestServerConfigBadCAPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewServerConfig(CertPaths{CA: filepath.Join(dir, "missing.crt")}); err == nil {
		t.Fatal("expected error for a missing CA file")
	}
}

func TestServerConfigMismatchedKeypairWrapsLabel(t *testing.T) {
	dir := t.TempDir()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := writePEM(t, dir, "ca.crt", ca.CertPEM)

	leaf, err := ca.issue(testLeafOptions{CommonName: "x", OU: "server", IsServer: true})
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, err := ca.issue(testLeafOptions{CommonName: "y", OU: "server", IsServer: true})
	if err != nil {
		t.Fatal(err)
	}
	certPath := writePEM(t, dir, "server.crt", leaf.CertPEM)
	mismatchedKeyPath := writePEM(t, dir, "mismatched.key", otherLeaf.KeyPEM)

	_, err = NewServerConfig(CertPaths{CA: caPath, Cert: certPath, Key: mismatchedKeyPath})
	if err == nil {
		t.Fatal("expected error for a mismatched cert/key pair")
	}
	if !strings.Contains(err.Error(), "server keypair") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "server keypair")
	}
}

func TestClientConfigMismatchedKeypairWrapsLabel(t *testing.T) {
	dir := t.TempDir()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := writePEM(t, dir, "ca.crt", ca.CertPEM)

	leaf, err := ca.issue(testLeafOptions{CommonName: "x", OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, err := ca.issue(testLeafOptions{CommonName: "y", OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	certPath := writePEM(t, dir, "tunnel.crt", leaf.CertPEM)
	mismatchedKeyPath := writePEM(t, dir, "mismatched.key", otherLeaf.KeyPEM)

	_, err = NewClientConfig(CertPaths{CA: caPath, Cert: certPath, Key: mismatchedKeyPath}, "")
	if err == nil {
		t.Fatal("expected error for a mismatched cert/key pair")
	}
	if !strings.Contains(err.Error(), "client keypair") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "client keypair")
	}
}

func TestLoadCAPoolErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCAPool(filepath.Join(dir, "missing.crt")); err == nil {
		t.Fatal("expected error for a missing CA file")
	}

	garbage := writePEM(t, dir, "garbage.crt", []byte("not a certificate"))
	if _, err := LoadCAPool(garbage); err == nil {
		t.Fatal("expected error for a CA file with no certificates")
	}
}
