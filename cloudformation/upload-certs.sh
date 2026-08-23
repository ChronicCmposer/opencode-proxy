#!/usr/bin/env bash
# The instance only fetches these SSM parameters at boot — after
# re-uploading a renewed cert, reboot it (or `systemctl restart
# opencode-proxy` after re-fetching by hand) to pick it up.
#
# Usage: cloudformation/upload-certs.sh [param-prefix]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

prefix="${1:-/opencode-proxy/server}"

put() {
  local name="$1" file="$2"
  aws ssm put-parameter --type SecureString --overwrite \
    --name "$prefix/$name" --value "file://$file"
}

put ca.crt pki/out/ca.crt
put server.crt pki/out/server.crt
put server.key pki/out/server.key

echo "uploaded ca.crt, server.crt, server.key under $prefix"
