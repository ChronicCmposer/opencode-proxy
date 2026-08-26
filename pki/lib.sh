# Shared helpers for the pki/*.sh scripts. Sourced, not executed directly.
set -euo pipefail

PKI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${PKI_OUT_DIR:-$PKI_DIR/out}"

CA_KEY="$OUT_DIR/ca.key"
CA_CERT="$OUT_DIR/ca.crt"

# Must match internal/tlsconf.OUTunnel / OUDevice — nothing enforces this.
OU_TUNNEL="opencode-proxy-tunnel"
OU_DEVICE="opencode-proxy-device"

require_ca() {
  if [[ ! -f "$CA_KEY" || ! -f "$CA_CERT" ]]; then
    echo "error: CA not found in $OUT_DIR — run init-ca.sh first" >&2
    exit 1
  fi
}

mkout() {
  mkdir -p "$OUT_DIR"
  # Everything under out/ is secret key material (the CA key, device p12s,
  # and the plaintext-password .mobileconfig profiles); keep the directory
  # owner-only so a stray world-readable umask can't expose them.
  chmod 700 "$OUT_DIR"
}

# issue_leaf <name> <subject-CN> <OU-or-empty> <eku> [SAN,SAN,...] [days]
# eku is "serverAuth" or "clientAuth". days defaults to 90; device certs pass
# a shorter lifetime (see issue-client.sh) because a device credential is the
# most-copied, highest-risk one — it rides on phones and inside an AirDropped
# .mobileconfig — so a shorter window caps how long a leaked one stays valid
# even before it is explicitly revoked.
issue_leaf() {
  local name="$1" cn="$2" ou="$3" eku="$4" sans="${5:-}" days="${6:-90}"
  require_ca
  mkout

  local key="$OUT_DIR/$name.key"
  local csr="$OUT_DIR/$name.csr"
  local crt="$OUT_DIR/$name.crt"
  local extfile
  extfile="$(mktemp)"
  trap 'rm -f "$extfile"' RETURN

  local subj="/O=opencode-proxy/CN=$cn"
  if [[ -n "$ou" ]]; then
    subj="/O=opencode-proxy/OU=$ou/CN=$cn"
  fi

  {
    echo "basicConstraints = CA:false"
    echo "keyUsage = critical, digitalSignature"
    echo "extendedKeyUsage = $eku"
    echo "subjectKeyIdentifier = hash"
    echo "authorityKeyIdentifier = keyid,issuer"
    if [[ -n "$sans" ]]; then
      echo "subjectAltName = $sans"
    fi
  } > "$extfile"

  openssl ecparam -name prime256v1 -genkey -noout -out "$key"
  # Restrict the private key's mode immediately, the same way init-ca.sh
  # guards the CA key: OUT_DIR is 700, but the .key files travel (uploaded to
  # SSM, copied to endpoints, occasionally moved out of out/), and a
  # world-readable mode would expose them the moment they leave this dir.
  chmod 600 "$key"
  openssl req -new -key "$key" -subj "$subj" -out "$csr"

  openssl x509 -req -in "$csr" -CA "$CA_CERT" -CAkey "$CA_KEY" -CAcreateserial \
    -days "$days" -sha256 -extfile "$extfile" -out "$crt"

  rm -f "$csr"
  echo "issued: $crt ($key)"
}
