// Package tlsconf builds the mutual-TLS configurations used by both halves
// of the proxy, and enforces the certificate identity split between tunnel
// endpoints and browser devices. mTLS alone proves a peer belongs to the
// private CA, not which role it holds; the role is carried in the leaf's
// Organizational Unit, so a stolen device certificate can't impersonate the
// tunnel endpoint.
package tlsconf

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

// ServerConfig verifies chain-to-CA only; per-request role checking is
// RequireOU's job, not this.
func ServerConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12, // Safari on older iOS still negotiates 1.2.
	}, nil
}

func ClientConfig(caPath, certPath, keyPath, serverName string) (*tls.Config, error) {
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func PeerOUs(state *tls.ConnectionState) []string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0].Subject.OrganizationalUnit
}

func PeerName(state *tls.ConnectionState) string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return "<none>"
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// RequireOU assumes chain verification already happened (via ServerConfig's
// ClientAuth) — it only checks role.
func RequireOU(state *tls.ConnectionState, ou string) error {
	if state == nil || len(state.PeerCertificates) == 0 {
		return ErrWrongRole
	}
	if !slices.Contains(PeerOUs(state), ou) {
		return fmt.Errorf("%w: have %v, need %q", ErrWrongRole, PeerOUs(state), ou)
	}
	return nil
}
