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

// loadPoolAndCert is the CA-pool-plus-keypair loading shared by
// ServerConfig and ClientConfig; label only affects the load error message.
func loadPoolAndCert(caPath, certPath, keyPath, label string) (*x509.CertPool, tls.Certificate, error) {
	pool, err := LoadCAPool(caPath)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("load %s keypair: %w", label, err)
	}
	return pool, cert, nil
}

// ServerConfig verifies chain-to-CA only; per-request role checking is
// RequireOU's job, not this.
func ServerConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(caPath, certPath, keyPath, "server")
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

func ClientConfig(caPath, certPath, keyPath, serverName string) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(caPath, certPath, keyPath, "client")
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

// peerCert returns state's leaf peer certificate, or nil if state carries
// none — the single place that defines what "no peer cert" means for
// PeerOUs, PeerName, and RequireOU.
func peerCert(state *tls.ConnectionState) *x509.Certificate {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0]
}

func PeerOUs(state *tls.ConnectionState) []string {
	cert := peerCert(state)
	if cert == nil {
		return nil
	}
	return cert.Subject.OrganizationalUnit
}

func PeerName(state *tls.ConnectionState) string {
	cert := peerCert(state)
	if cert == nil {
		return "<none>"
	}
	return cert.Subject.CommonName
}

// RequireOU assumes chain verification already happened (via ServerConfig's
// ClientAuth) — it only checks role.
func RequireOU(state *tls.ConnectionState, ou string) error {
	if peerCert(state) == nil {
		return ErrWrongRole
	}
	if !slices.Contains(PeerOUs(state), ou) {
		return fmt.Errorf("%w: have %v, need %q", ErrWrongRole, PeerOUs(state), ou)
	}
	return nil
}
