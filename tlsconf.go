// The mutual-TLS configurations used by both halves of the proxy, and the
// certificate identity split between tunnel endpoints and browser devices.
// mTLS alone proves a peer belongs to the private CA, not which role it
// holds; the role is carried in the leaf's Organizational Unit, so a
// stolen device certificate can't impersonate the tunnel endpoint.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
)

// Must match the OU values pki/issue-tunnel.sh and pki/issue-client.sh
// actually issue — nothing enforces this at compile time.
const (
	OUTunnel = "opencode-proxy-tunnel"
	OUDevice = "opencode-proxy-device"
)

var ErrWrongRole = errors.New("client certificate is not valid for this endpoint")

// ErrRevoked is returned when a peer presents a certificate whose serial
// appears in the revocation list. It fails the TLS handshake, so it applies to
// tunnel and device certs alike, before any request-level role check.
var ErrRevoked = errors.New("client certificate has been revoked")

// RevokedSerials is the set of certificate serial numbers that must be
// rejected even though they still chain to the CA and haven't expired. The
// codebase issues no CRL/OCSP responder, so this file-backed denylist is what
// lets a lost phone's certificate be cut off without rotating the whole CA:
// list its serial here and every endpoint refuses it at the next handshake.
// The keys are lowercase hex with no separators, matching how
// LoadRevokedSerials normalizes both the file and each peer certificate.
type RevokedSerials map[string]struct{}

// LoadRevokedSerials reads a revocation list: one certificate serial per line
// as hex (colons, whitespace and a leading 0x are ignored so the output of
// `openssl x509 -noout -serial` can be pasted in directly), with blank lines
// and # comments skipped. An empty path yields an empty set, so revocation is
// opt-in and a deployment without a list still starts.
func LoadRevokedSerials(path string) (RevokedSerials, error) {
	revoked := RevokedSerials{}
	if path == "" {
		return revoked, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read revocation list: %w", err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		norm, err := normalizeSerial(line)
		if err != nil {
			return nil, fmt.Errorf("revocation list %s: %w", path, err)
		}
		revoked[norm] = struct{}{}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("read revocation list %s: %w", path, err)
	}
	return revoked, nil
}

// normalizeSerial renders a certificate serial as lowercase hex with no
// separators, the canonical form both the file entries and live peer
// certificates are compared in.
func normalizeSerial(s string) (string, error) {
	cleaned := strings.ToLower(s)
	cleaned = strings.TrimPrefix(cleaned, "0x")
	cleaned = strings.ReplaceAll(cleaned, ":", "")
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	n, ok := new(big.Int).SetString(cleaned, 16)
	if !ok {
		return "", fmt.Errorf("not a hex serial: %q", s)
	}
	return n.Text(16), nil
}

// serialOf renders a certificate's serial in the same canonical form as
// normalizeSerial, so a denylist entry matches regardless of formatting.
func serialOf(cert *x509.Certificate) string {
	return cert.SerialNumber.Text(16)
}

// minTLSVersion is the floor for both server and client configs: Safari on
// older iOS still negotiates 1.2, so anything issued for a device
// certificate needs 1.2 to keep working.
const minTLSVersion = tls.VersionTLS12

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

// NewServerTLSConfig verifies chain-to-CA and rejects any peer certificate
// whose serial is in revoked; per-request role checking is VerifyPeerRole's
// job, not this. revoked may be empty, in which case no certificate is
// rejected on revocation grounds.
func NewServerTLSConfig(certs CertPaths, revoked RevokedSerials) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(certs, "server")
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   minTLSVersion,
		// VerifyConnection runs after the chain is verified, so
		// VerifiedChains[0][0] is the peer leaf. Rejecting here fails the
		// handshake itself — a revoked cert never reaches a request handler,
		// and the check covers both tunnel and device certs uniformly.
		VerifyConnection: verifyNotRevoked(revoked),
	}, nil
}

// verifyNotRevoked builds the VerifyConnection callback that rejects a peer
// whose leaf serial is listed in revoked. It returns nil (no callback) when
// the list is empty, so an unconfigured deployment pays nothing.
func verifyNotRevoked(revoked RevokedSerials) func(tls.ConnectionState) error {
	if len(revoked) == 0 {
		return nil
	}
	return func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
			return ErrRevoked
		}
		leaf := cs.VerifiedChains[0][0]
		if _, bad := revoked[serialOf(leaf)]; bad {
			return fmt.Errorf("%w: serial %s (%q)", ErrRevoked, serialOf(leaf), leaf.Subject.CommonName)
		}
		return nil
	}
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

func leafCertOf(state *tls.ConnectionState) *x509.Certificate {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0]
}

// GetPeerSubjectCN never fails — it exists for log lines, and an
// unidentified peer is still worth logging.
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
