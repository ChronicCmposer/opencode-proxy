#!/usr/bin/env bash
# OU=opencode-proxy-tunnel is what lets internal/remote's tlsconf.RequireOU
# tell this cert apart from a device cert — a stolen phone cert can't use
# this to open the /_tunnel endpoint.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

name="${1:-home-mac}"
issue_leaf "tunnel" "$name" "$OU_TUNNEL" "clientAuth"
echo "deploy tunnel.crt + tunnel.key to the Mac; pass to opencode-proxy --local via --cert/--key"
