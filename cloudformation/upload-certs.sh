#!/usr/bin/env bash
# Uploads the remote server's cert material into SSM Parameter Store as
# SecureStrings, at the paths the stack's UserData reads from. Run this
# after pki/issue-server.sh and before (or after re-running) deploy.sh —
# the instance re-fetches these only at boot, so update this then reboot
# (or `systemctl restart opencode-proxy` after re-fetching by hand) to pick
# up a renewed cert.
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
