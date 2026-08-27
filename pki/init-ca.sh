#!/usr/bin/env bash
# Run once. Keep pki/out/ca.key offline as much as practical — it's the
# root of trust for everything reachable through the tunnel.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ -f "$CA_KEY" ]]; then
  echo "error: $CA_KEY already exists — refusing to overwrite an existing CA" >&2
  echo "delete pki/out/ manually first if you really want a fresh CA (this invalidates every issued cert)" >&2
  exit 1
fi

mkout

openssl ecparam -name prime256v1 -genkey -noout -out "$CA_KEY"
chmod 600 "$CA_KEY"

openssl req -x509 -new -key "$CA_KEY" -days 3650 -sha256 \
  -config "$PKI_DIR/openssl.cnf" -extensions v3_ca \
  -out "$CA_CERT"

echo "CA created:"
echo "  key:  $CA_KEY  (keep private, do not commit)"
echo "  cert: $CA_CERT (distribute to every endpoint as --ca)"
