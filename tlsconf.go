// The mutual-TLS configurations used by both halves of the proxy, and the
// certificate identity split between tunnel endpoints and browser devices.
// mTLS alone proves a peer belongs to the private CA, not which role it
// holds; the role is carried in the leaf's Organizational Unit, so a
// stolen device certificate can't impersonate the tunnel endpoint.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
)

// Must match the OU values pki/issue-tunnel.sh and pki/issue-client.sh
// actually issue — nothing enforces this at compile time.
const (
	OUTunnel = "opencode-proxy-tunnel"
	OUDevice = "opencode-proxy-device"
)

var ErrWrongRole = errors.New("client certificate is not valid for this endpoint")

// CertPaths groups the three PEM paths that always travel together: nothing
// loads one without the other two, and passing them separately cost every
// function along the wiring path three positional string parameters.
type CertPaths struct {
	CA   string
	Cert string
	Key  string
}

func LoadCAPool(caPath string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in CA bundle %s", caPath)
	}
	return pool, nil
}

// loadPoolAndCert loads the CA pool and keypair shared by NewServerTLSConfig
// and NewClientTLSConfig; label only appears in the load error.
func loadPoolAndCert(certs CertPaths, label string) (*x509.CertPool, tls.Certificate, error) {
	pool, err := LoadCAPool(certs.CA)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	cert, err := tls.LoadX509KeyPair(certs.Cert, certs.Key)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("load %s keypair: %w", label, err)
	}
	return pool, cert, nil
}

// NewServerTLSConfig verifies chain-to-CA only; per-request role checking is
// VerifyPeerRole's job, not this.
func NewServerTLSConfig(certs CertPaths) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(certs, "server")
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12, // Safari on older iOS still negotiates 1.2.
	}, nil
}

func NewClientTLSConfig(certs CertPaths, serverName string) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(certs, "client")
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// leafCertOf returns state's leaf peer certificate, or nil if state carries
// none.
func leafCertOf(state *tls.ConnectionState) *x509.Certificate {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0]
}

// GetPeerSubjectCN returns the peer leaf certificate's Subject Common Name,
// or "<none>" when there is no peer certificate. It exists for log lines, so
// it never fails — an unidentified peer is still worth logging.
func GetPeerSubjectCN(state *tls.ConnectionState) string {
	cert := leafCertOf(state)
	if cert == nil {
		return "<none>"
	}
	return cert.Subject.CommonName
}

// VerifyPeerRole reports whether the peer's certificate carries the ou role,
// assuming chain verification already happened (via NewServerTLSConfig's
// ClientAuth) — it only checks role.
func VerifyPeerRole(state *tls.ConnectionState, ou string) error {
	cert := leafCertOf(state)
	if cert == nil {
		return ErrWrongRole
	}
	if !slices.Contains(cert.Subject.OrganizationalUnit, ou) {
		return fmt.Errorf("%w: have %v, need %q", ErrWrongRole, cert.Subject.OrganizationalUnit, ou)
	}
	return nil
}
