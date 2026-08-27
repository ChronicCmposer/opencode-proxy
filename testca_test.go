// An in-memory CA and leaf certificates for tests, so the test suite
// doesn't depend on the openssl scripts in pki/.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

type testCA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
}

func newTestCA() (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "opencode-proxy test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &testCA{Cert: cert, CertPEM: pemEncode("CERTIFICATE", der), key: key}, nil
}

type testLeafOptions struct {
	CommonName string
	OU         string
	DNSNames   []string
	IsServer   bool // sets serverAuth EKU in addition to clientAuth
}

type testLeaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

func (ca *testCA) issue(opts testLeafOptions) (*testLeaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if opts.IsServer {
		eku = append(eku, x509.ExtKeyUsageServerAuth)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:         opts.CommonName,
			OrganizationalUnit: []string{opts.OU},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: eku,
		DNSNames:    opts.DNSNames,
	}
	for _, name := range opts.DNSNames {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &testLeaf{
		CertPEM: pemEncode("CERTIFICATE", der),
		KeyPEM:  pemEncode("EC PRIVATE KEY", keyDER),
	}, nil
}

// issueDevice and issueTunnel cover the common case across the test suite —
// a leaf with only a CommonName and role OU — so callers don't have to
// repeat the testLeafOptions literal and error check at every call site.
func (ca *testCA) issueDevice(t *testing.T, cn string) *testLeaf {
	t.Helper()
	leaf, err := ca.issue(testLeafOptions{CommonName: cn, OU: OUDevice})
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func (ca *testCA) issueTunnel(t *testing.T, cn string) *testLeaf {
	t.Helper()
	leaf, err := ca.issue(testLeafOptions{CommonName: cn, OU: OUTunnel})
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func (l *testLeaf) tlsCert() (tls.Certificate, error) {
	return tls.X509KeyPair(l.CertPEM, l.KeyPEM)
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
