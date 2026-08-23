#!/usr/bin/env bash
# Issues the tunnel client certificate: the Mac's identity when it dials
# `opencode-proxy --local` out to the remote. Only a certificate carrying
# OU=opencode-proxy-tunnel is permitted to open the /_tunnel endpoint
# (enforced in internal/remote via tlsconf.RequireOU) — a stolen phone
# certificate cannot be used to impersonate the tunnel.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

name="${1:-home-mac}"
issue_leaf "tunnel" "$name" "$OU_TUNNEL" "clientAuth"
echo "deploy tunnel.crt + tunnel.key to the Mac; pass to opencode-proxy --local via --cert/--key"
