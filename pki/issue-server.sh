#!/usr/bin/env bash
# Usage: issue-server.sh <domain> [more.domain ...]
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <domain> [more.domain ...]" >&2
  exit 1
fi

primary="$1"
sans=""
for d in "$@"; do
  if [[ "$d" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    sans+="${sans:+,}IP:$d"
  else
    sans+="${sans:+,}DNS:$d"
  fi
done

issue_leaf "server" "$primary" "" "serverAuth" "$sans"
echo "deploy server.crt + server.key to the remote host (see cloudformation/ SSM parameters)"
