#!/usr/bin/env bash
# Leaf certificates are issued with a 90-day lifetime. Re-run the relevant
# issue-*.sh script before expiry to renew:
#
#   pki/issue-server.sh code.example.com   # then redeploy to the remote host
#   pki/issue-tunnel.sh                    # then redeploy to the Mac
#   pki/issue-client.sh phone              # then reinstall the .mobileconfig
#
# Check current expiry with:
#   openssl x509 -in pki/out/<name>.crt -noout -enddate
#
# This script just reports what's expiring soon; it does not renew
# automatically (renewal touches devices you may not have to hand).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

warn_days="${1:-14}"

shopt -s nullglob
for crt in "$OUT_DIR"/*.crt; do
  [[ "$(basename "$crt")" == "ca.crt" ]] && continue
  end_epoch="$(openssl x509 -in "$crt" -noout -enddate | cut -d= -f2 | xargs -I{} date -d {} +%s 2>/dev/null || true)"
  [[ -z "$end_epoch" ]] && continue
  now_epoch="$(date +%s)"
  days_left=$(( (end_epoch - now_epoch) / 86400 ))
  if (( days_left <= warn_days )); then
    echo "EXPIRING: $crt in $days_left day(s) — re-run the matching issue-*.sh script"
  fi
done
