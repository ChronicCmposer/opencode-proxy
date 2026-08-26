#!/usr/bin/env bash
# Only reports what's expiring soon — despite the name, it does not renew
# anything itself (renewal touches devices you may not have to hand).
# Re-run the matching issue-*.sh script to actually renew.
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
