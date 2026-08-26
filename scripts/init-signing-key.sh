#!/usr/bin/env bash
# Run once, on a trusted machine, to establish the release signing key.
#
# The image tar published to GitHub Releases is code that every host runs as
# root (see scripts/update-image.sh). A same-origin checksum only proves the
# download wasn't corrupted — anyone who can publish a release can supply a
# matching checksum, so it is no defense against a tampered or malicious
# release. This key closes that gap: releases are signed with the private half
# (kept offline, like the CA key), and every host verifies against the public
# half committed to the repo at scripts/release-signing.pub before importing.
#
# The private key is written outside the repo tree (gitignored location) and
# must be guarded like pki/out/ca.key. Losing it means re-running this script
# and committing the new public key; leaking it means an attacker can forge
# releases, so treat it accordingly.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

KEY_DIR="${RELEASE_SIGNING_DIR:-signing}"
PRIV="$KEY_DIR/release.key"
PUB="scripts/release-signing.pub"

if [[ -f "$PRIV" ]]; then
  echo "error: $PRIV already exists — refusing to overwrite the signing key" >&2
  echo "delete it manually if you really want a new key (invalidates verification until the new $PUB is deployed everywhere)" >&2
  exit 1
fi

mkdir -p "$KEY_DIR"
chmod 700 "$KEY_DIR"

openssl genpkey -algorithm ed25519 -out "$PRIV"
chmod 600 "$PRIV"
openssl pkey -in "$PRIV" -pubout -out "$PUB"

echo "signing key created:"
echo "  private: $PRIV  (keep offline, never commit — like pki/out/ca.key)"
echo "  public:  $PUB    (commit this; hosts verify releases against it)"
echo
echo "next: commit $PUB, then run 'make release' (which signs with $PRIV)."
