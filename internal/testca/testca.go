// Package testca generates an in-memory CA and leaf certificates for tests,
// so the test suite doesn't depend on the openssl scripts in pki/.
package testca

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
	"time"
)

// CA is a throwaway certificate authority for tests.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
}

// New creates a fresh self-signed CA.
func New() (*CA, error) {
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
	return &CA{Cert: cert, CertPEM: pemEncode("CERTIFICATE", der), key: key}, nil
}

// LeafOptions configures an issued leaf certificate.
type LeafOptions struct {
	CommonName string
	OU         string
	DNSNames   []string
	IsServer   bool // sets serverAuth EKU in addition to clientAuth
}

// Leaf is an issued end-entity certificate as PEM bytes, ready for
// tls.X509KeyPair or writing to disk.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Issue signs a new leaf certificate under the CA.
func (ca *CA) Issue(opts LeafOptions) (*Leaf, error) {
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
	return &Leaf{
		CertPEM: pemEncode("CERTIFICATE", der),
		KeyPEM:  pemEncode("EC PRIVATE KEY", keyDER),
	}, nil
}

// TLSCert returns the leaf as a tls.Certificate.
func (l *Leaf) TLSCert() (tls.Certificate, error) {
	return tls.X509KeyPair(l.CertPEM, l.KeyPEM)
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
