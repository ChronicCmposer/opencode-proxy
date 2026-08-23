#!/usr/bin/env bash
# Checks whether a newer opencode-proxy image has been published and, if
# so, imports it and restarts the service — with an automatic rollback if
# the new image doesn't stay up. Shared by both hosts: the EC2 --remote
# instance and the local --local VM both run this under the same
# opencode-proxy-update.timer/.service pair (every 6h), pointed at
# different services via /etc/opencode-proxy/update.env.
#
# Every 6h this only downloads a small checksum file, not the image itself
# — the full opencode-proxy.tar is only pulled when that checksum actually
# changes.
#
# Configuration comes from the environment (see the .service unit's
# EnvironmentFile=):
#   IMAGE_TAR_URL   e.g. https://github.com/.../releases/latest/download/opencode-proxy.tar
#   IMAGE_REF       e.g. docker.io/library/opencode-proxy:latest
#   SERVICE_NAME    e.g. opencode-proxy or opencode-proxy-local
#   STATE_DIR       default /etc/opencode-proxy
set -euo pipefail

: "${IMAGE_TAR_URL:?IMAGE_TAR_URL must be set}"
: "${IMAGE_REF:?IMAGE_REF must be set}"
: "${SERVICE_NAME:?SERVICE_NAME must be set}"
STATE_DIR="${STATE_DIR:-/etc/opencode-proxy}"

CURRENT_SHA_FILE="$STATE_DIR/current.sha256"
ROLLBACK_REF="${IMAGE_REF}-rollback"
# How long to wait after restarting before deciding the update "took."
# ctr run's ExecStart runs in the foreground, so `systemctl is-active`
# genuinely reflects whether the container process is still alive.
HEALTH_WAIT_SECS=15

log() { echo "opencode-proxy-update: $*"; }

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

curl -fsSL -o "$work_dir/tar.sha256" "${IMAGE_TAR_URL}.sha256"
new_sha="$(awk '{print $1}' "$work_dir/tar.sha256")"
current_sha="$(cat "$CURRENT_SHA_FILE" 2>/dev/null || echo none)"

if [[ "$current_sha" == "$new_sha" ]]; then
  log "already up to date ($new_sha)"
  exit 0
fi
log "new image available (have $current_sha, want $new_sha) — updating"

curl -fsSL -o "$work_dir/image.tar" "$IMAGE_TAR_URL"
downloaded_sha="$(sha256sum "$work_dir/image.tar" | awk '{print $1}')"
if [[ "$downloaded_sha" != "$new_sha" ]]; then
  log "error: downloaded tar checksum ($downloaded_sha) doesn't match published checksum ($new_sha) — aborting, nothing changed"
  exit 1
fi

# Snapshot the currently-running image under a rollback tag before
# replacing it. Ignored if there's nothing to snapshot yet (first run).
ctr images tag "$IMAGE_REF" "$ROLLBACK_REF" >/dev/null 2>&1 || true

ctr images import "$work_dir/image.tar"
systemctl restart "$SERVICE_NAME"
sleep "$HEALTH_WAIT_SECS"

if systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "$new_sha" > "$CURRENT_SHA_FILE"
  log "updated to $new_sha; $SERVICE_NAME is running"
  exit 0
fi

log "error: $SERVICE_NAME did not stay up after updating — attempting rollback"
if ctr images tag "$ROLLBACK_REF" "$IMAGE_REF" >/dev/null 2>&1; then
  systemctl restart "$SERVICE_NAME"
  sleep "$HEALTH_WAIT_SECS"
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    log "rolled back successfully; $SERVICE_NAME is running the previous image again"
  else
    log "error: $SERVICE_NAME still not running after rollback — needs manual attention (journalctl -u $SERVICE_NAME)"
  fi
else
  log "error: no previous image available to roll back to (first-ever update?) — needs manual attention"
fi
exit 1
