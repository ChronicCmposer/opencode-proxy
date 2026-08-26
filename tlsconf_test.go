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
	tunnelLeaf := ca.issueTunnel(t, "home-mac")
	deviceLeaf := ca.issueDevice(t, "phone")
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

	serverConf, err := NewServerTLSConfig(CertPaths{CA: caPath, Cert: serverCertPath, Key: serverKeyPath}, nil)
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

	tunnelLeaf := ca.issueTunnel(t, "home")
	tunnelCertPath := writePEM(t, dir, "tunnel.crt", tunnelLeaf.CertPEM)
	tunnelKeyPath := writePEM(t, dir, "tunnel.key", tunnelLeaf.KeyPEM)

	clientConf, err := NewClientTLSConfig(CertPaths{CA: caPath, Cert: tunnelCertPath, Key: tunnelKeyPath}, "127.0.0.1")
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
	if _, err := NewServerTLSConfig(CertPaths{CA: filepath.Join(dir, "missing.crt")}, nil); err == nil {
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

	_, err = NewServerTLSConfig(CertPaths{CA: caPath, Cert: certPath, Key: mismatchedKeyPath}, nil)
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

	leaf := ca.issueTunnel(t, "x")
	otherLeaf := ca.issueTunnel(t, "y")
	certPath := writePEM(t, dir, "tunnel.crt", leaf.CertPEM)
	mismatchedKeyPath := writePEM(t, dir, "mismatched.key", otherLeaf.KeyPEM)

	_, err = NewClientTLSConfig(CertPaths{CA: caPath, Cert: certPath, Key: mismatchedKeyPath}, "")
	if err == nil {
		t.Fatal("expected error for a mismatched cert/key pair")
	}
	if !strings.Contains(err.Error(), "client keypair") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "client keypair")
	}
}

func certFromLeaf(t *testing.T, leaf *testLeaf) *x509.Certificate {
	t.Helper()
	tc, err := leaf.tlsCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(tc.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestLoadRevokedSerials(t *testing.T) {
	dir := t.TempDir()

	if got, err := LoadRevokedSerials(""); err != nil || len(got) != 0 {
		t.Fatalf("LoadRevokedSerials(\"\") = %v, %v; want empty set, nil", got, err)
	}

	// Mixed formats, comments and blanks — all must normalize to the same set.
	path := writePEM(t, dir, "revoked.txt", []byte(`
# a lost phone
DE:AD:BE:EF
0x00ff
  1a2b3c

`))
	revoked, err := LoadRevokedSerials(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deadbeef", "ff", "1a2b3c"} {
		if _, ok := revoked[want]; !ok {
			t.Errorf("serial %q not in revocation set %v", want, revoked)
		}
	}

	bad := writePEM(t, dir, "bad.txt", []byte("not-hex-zzz"))
	if _, err := LoadRevokedSerials(bad); err == nil {
		t.Fatal("expected an error for a non-hex serial")
	}
}

func TestVerifyNotRevoked(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	revokedLeaf := ca.issueDevice(t, "lost-phone")
	goodLeaf := ca.issueDevice(t, "trusted-phone")

	revokedCert := certFromLeaf(t, revokedLeaf)
	goodCert := certFromLeaf(t, goodLeaf)

	dir := t.TempDir()
	path := writePEM(t, dir, "revoked.txt", []byte(serialOf(revokedCert)+"\n"))
	revocation, err := NewRevocationList(path)
	if err != nil {
		t.Fatal(err)
	}

	// A nil list (--revoked omitted) installs no callback.
	if verifyNotRevoked(nil) != nil {
		t.Error("verifyNotRevoked(nil) should return no callback")
	}

	check := verifyNotRevoked(revocation)
	if check == nil {
		t.Fatal("verifyNotRevoked with a configured list should return a callback")
	}

	// A revoked leaf is rejected; a good one passes.
	revokedState := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{revokedCert}}}
	if err := check(revokedState); err == nil {
		t.Error("revoked certificate should be rejected")
	}
	goodState := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{goodCert}}}
	if err := check(goodState); err != nil {
		t.Errorf("non-revoked certificate should pass, got %v", err)
	}
	// A connection with no verified chain is rejected rather than trusted.
	if err := check(tls.ConnectionState{}); err == nil {
		t.Error("empty verified chain should be rejected")
	}
}

// TestRevocationListReload is the V1 regression: a serial added to the file
// after the list is built must take effect at the next check, with no reload
// call of the caller's own — the deployment ships an empty revoked.txt and
// promises "no redeploy needed" when a device is lost.
func TestRevocationListReload(t *testing.T) {
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	lostCert := certFromLeaf(t, ca.issueDevice(t, "lost-phone"))

	dir := t.TempDir()
	// Start with an empty list, exactly as cloudformation/stack.yaml's
	// `touch revoked.txt` leaves it.
	path := writePEM(t, dir, "revoked.txt", []byte(""))
	revocation, err := NewRevocationList(path)
	if err != nil {
		t.Fatal(err)
	}
	if revocation.IsRevoked(serialOf(lostCert)) {
		t.Fatal("serial should not be revoked before it is listed")
	}

	// Rewrite the file with the lost device's serial. A same-mtime write can
	// hide behind the second-granularity timestamp, but the size changes from
	// 0, which IsRevoked also keys on.
	if err := os.WriteFile(path, []byte(serialOf(lostCert)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !revocation.IsRevoked(serialOf(lostCert)) {
		t.Error("serial added to the file after startup should be revoked at the next check")
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
