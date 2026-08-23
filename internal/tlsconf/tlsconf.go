// Package tlsconf builds the mutual-TLS configurations used by both halves of
// the proxy, and enforces the certificate identity split between tunnel
// endpoints and browser devices.
//
// Every participant — the remote proxy, the local proxy, and each browser
// device — holds a leaf certificate signed by a single private CA. mTLS alone
// proves a peer belongs to that CA, but not which role it holds. The role is
// carried in the leaf's Organizational Unit, so that a stolen device
// certificate cannot be used to impersonate the tunnel endpoint.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
)

// Organizational Unit values that distinguish the two client roles. Issued by
// the pki/ scripts: issue-tunnel.sh sets OUTunnel, issue-client.sh sets ODevice.
const (
	OUTunnel = "opencode-proxy-tunnel"
	OUDevice = "opencode-proxy-device"
)

// ErrWrongRole is returned when a peer presents a valid certificate for a role
// other than the one the endpoint requires.
var ErrWrongRole = errors.New("client certificate is not valid for this endpoint")

// LoadCAPool reads a PEM bundle of CA certificates into a pool.
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

// ServerConfig builds the config for the remote proxy's public listener. Every
// connection must present a certificate chaining to the private CA; the
// per-role check happens later, per-request, via RequireOU.
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

// ClientConfig builds the config the local proxy uses to dial the remote. The
// server's certificate is signed by the private CA rather than a public one,
// so the CA pool is supplied explicitly as RootCAs.
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

// PeerOUs returns the Organizational Units on the verified peer certificate of
// a completed handshake.
func PeerOUs(state *tls.ConnectionState) []string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0].Subject.OrganizationalUnit
}

// PeerName returns the Common Name of the verified peer certificate, for logs.
func PeerName(state *tls.ConnectionState) string {
	if state == nil || len(state.PeerCertificates) == 0 {
		return "<none>"
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// RequireOU reports whether the peer holds the given role. The TLS stack has
// already verified the chain by the time this runs, so this is purely the
// role check.
func RequireOU(state *tls.ConnectionState, ou string) error {
	if state == nil || len(state.PeerCertificates) == 0 {
		return ErrWrongRole
	}
	if !slices.Contains(PeerOUs(state), ou) {
		return fmt.Errorf("%w: have %v, need %q", ErrWrongRole, PeerOUs(state), ou)
	}
	return nil
}
