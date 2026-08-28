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
	"strconv"
	"strings"
	"sync"
	"time"
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

// errNoVerifiedChain is returned when VerifyConnection runs with no verified
// chain to inspect. It is distinct from ErrRevoked on purpose: an unverified
// chain is not a revocation, and conflating the two would let errors.Is
// misreport a defensive fallback as a genuine revocation. ClientAuth's
// RequireAndVerifyClientCert makes this branch unreachable in practice, but
// the callback should still not lie if it ever fires.
var errNoVerifiedChain = errors.New("no verified chain to check")

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
	cleaned := strings.ToLower(strings.TrimSpace(s))
	// `openssl x509 -noout -serial` prints "serial=AB12CD"; strip that prefix
	// so its output pastes in directly, as the LoadRevokedSerials doc promises.
	cleaned = strings.TrimPrefix(cleaned, "serial=")
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

// RevocationList evaluates certificate revocation against a file that may
// change while the server runs. A file-backed denylist is only worth anything
// if a serial added after startup takes effect without a restart: revocation
// is the sole way to cut off a stolen device certificate, and the deployment
// (see cloudformation/stack.yaml, which ships an initially *empty*
// revoked.txt) promises a lost device can be denied "no redeploy needed." So
// IsRevoked re-reads the file whenever its mtime or size has changed since the
// last check — a newly listed serial is refused at the very next handshake.
//
// An empty --revoked path opts out of revocation entirely: NewRevocationList
// returns nil and no VerifyConnection callback is installed, so an
// unconfigured deployment still pays nothing.
type RevocationList struct {
	path    string
	mu      sync.Mutex
	serials RevokedSerials
	modTime time.Time
	size    int64
}

// NewRevocationList loads the initial denylist from path and returns a list
// that reloads on change. A malformed file is rejected here at startup, the
// same fail-fast behavior LoadRevokedSerials had. An empty path means
// "revocation not configured" and yields (nil, nil): callers treat a nil list
// as "never revoke, install no callback."
func NewRevocationList(path string) (*RevocationList, error) {
	if path == "" {
		return nil, nil
	}
	serials, err := LoadRevokedSerials(path)
	if err != nil {
		return nil, err
	}
	rl := &RevocationList{path: path, serials: serials}
	if fi, err := os.Stat(path); err == nil {
		rl.modTime, rl.size = fi.ModTime(), fi.Size()
	}
	return rl, nil
}

// IsRevoked reports whether serial (canonical hex, per normalizeSerial) is on
// the current denylist, reloading the file first if it has changed on disk. A
// reload that fails to parse — a truncated file caught mid-edit, say — keeps
// the last good set rather than silently disabling revocation; a stat error
// (file temporarily missing) does the same.
func (r *RevocationList) IsRevoked(serial string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fi, err := os.Stat(r.path); err == nil {
		if fi.ModTime() != r.modTime || fi.Size() != r.size {
			if serials, err := LoadRevokedSerials(r.path); err == nil {
				r.serials = serials
				r.modTime, r.size = fi.ModTime(), fi.Size()
			}
		}
	}
	_, bad := r.serials[serial]
	return bad
}

// minTLSVersion is the floor for both server and client configs: Safari on
// older iOS still negotiates 1.2, so anything issued for a device
// certificate needs 1.2 to keep working.
const minTLSVersion = tls.VersionTLS12

// tlsCipherSuites is the explicit cipher-suite allowlist for the TLS 1.2
// fallback. Go ignores CipherSuites for TLS 1.3 (whose suites are always
// AEAD and safe), so this only constrains 1.2, kept solely for older Safari:
// AEAD suites only — GCM and ChaCha20-Poly1305, no CBC and no RSA key
// exchange. Every certificate in this PKI is ECDSA P-256 (pki/lib.sh issues
// with `openssl ecparam -name prime256v1`), so only the ECDSA-authentication
// suites can ever be negotiated; pinning them documents that and forecloses
// the weaker defaults a public listener would otherwise still offer at 1.2.
var tlsCipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
}

// tlsCurvePreferences pins the ECDHE key-exchange curves to the two modern,
// constant-time options, X25519 first.
var tlsCurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256}

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
// whose serial is on revocation's denylist; per-request role checking is
// VerifyPeerRole's job, not this. revocation may be nil, in which case no
// certificate is rejected on revocation grounds.
func NewServerTLSConfig(certs CertPaths, revocation *RevocationList) (*tls.Config, error) {
	pool, cert, err := loadPoolAndCert(certs, "server")
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientCAs:        pool,
		ClientAuth:       tls.RequireAndVerifyClientCert,
		MinVersion:       minTLSVersion,
		CipherSuites:     tlsCipherSuites,
		CurvePreferences: tlsCurvePreferences,
		// VerifyConnection runs after the chain is verified, so
		// VerifiedChains[0][0] is the peer leaf. Rejecting here fails the
		// handshake itself — a revoked cert never reaches a request handler,
		// and the check covers both tunnel and device certs uniformly.
		// revocation re-reads its file per call, so a serial listed after the
		// server started is refused at this handshake, no restart needed.
		VerifyConnection: verifyNotRevoked(revocation),
	}, nil
}

// verifyNotRevoked builds the VerifyConnection callback that rejects a peer
// whose leaf serial is on revocation's (reloading) denylist. It returns nil
// (no callback) when revocation is nil — the --revoked flag was omitted — so
// an unconfigured deployment pays nothing.
func verifyNotRevoked(revocation *RevocationList) func(tls.ConnectionState) error {
	if revocation == nil {
		return nil
	}
	return func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
			return errNoVerifiedChain
		}
		leaf := cs.VerifiedChains[0][0]
		if revocation.IsRevoked(serialOf(leaf)) {
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
		Certificates:     []tls.Certificate{cert},
		RootCAs:          pool,
		ServerName:       serverName,
		MinVersion:       minTLSVersion,
		CipherSuites:     tlsCipherSuites,
		CurvePreferences: tlsCurvePreferences,
	}, nil
}

func leafCertOf(state *tls.ConnectionState) *x509.Certificate {
	if state == nil || len(state.PeerCertificates) == 0 {
		return nil
	}
	return state.PeerCertificates[0]
}

// sanitizeForLog renders s safe to interpolate into a single log line.
// Certificate Subject fields (CN, OU) are carried on a CA-signed cert, but
// their contents are still attacker-influenced: a cert whose CN embeds a
// newline or other control character could otherwise forge or split log
// entries (and any log-driven alerting built on top). strconv.Quote escapes
// every control character and wraps the value in quotes, collapsing it to one
// unambiguous token.
func sanitizeForLog(s string) string {
	return strconv.Quote(s)
}

// GetPeerSubjectCN never fails — it exists for log lines, and an
// unidentified peer is still worth logging. The CN is quoted and
// control-character-escaped (sanitizeForLog) since it comes off the peer's
// certificate and feeds straight into log output.
func GetPeerSubjectCN(state *tls.ConnectionState) string {
	cert := leafCertOf(state)
	if cert == nil {
		return "<none>"
	}
	return sanitizeForLog(cert.Subject.CommonName)
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
		// %q on the OU slice quotes each element, escaping any control
		// character a malicious-but-CA-signed cert might embed to forge the
		// log line this error is printed into.
		return fmt.Errorf("%w: have %q, need %q", ErrWrongRole, cert.Subject.OrganizationalUnit, ou)
	}
	return nil
}

// VerifyPeerCN reports whether the peer leaf's Common Name equals cn. It pins
// the tunnel endpoint's identity beyond its role: VerifyPeerRole proves a peer
// holds *a* tunnel-role certificate, but any holder of any such cert would
// pass it and could then seize the tunnel registry and man-in-the-middle every
// device request (see SessionRegistry.Set). Pinning the CN narrows that to the
// single enrolled home endpoint. Chain verification is assumed already done
// (NewServerTLSConfig's ClientAuth); this only checks identity.
func VerifyPeerCN(state *tls.ConnectionState, cn string) error {
	cert := leafCertOf(state)
	if cert == nil {
		return ErrWrongRole
	}
	if cert.Subject.CommonName != cn {
		return fmt.Errorf("%w: tunnel CN %q is not the pinned %q", ErrWrongRole, cert.Subject.CommonName, cn)
	}
	return nil
}
