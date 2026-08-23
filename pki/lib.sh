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
}

# issue_leaf <name> <subject-CN> <OU-or-empty> <eku> [SAN,SAN,...]
# eku is "serverAuth" or "clientAuth".
issue_leaf() {
  local name="$1" cn="$2" ou="$3" eku="$4" sans="${5:-}"
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
  openssl req -new -key "$key" -subj "$subj" -out "$csr"

  openssl x509 -req -in "$csr" -CA "$CA_CERT" -CAkey "$CA_KEY" -CAcreateserial \
    -days 90 -sha256 -extfile "$extfile" -out "$crt"

  rm -f "$csr"
  echo "issued: $crt ($key)"
}
