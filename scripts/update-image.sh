#!/usr/bin/env bash
# Configuration comes from the environment (see the .service unit's
# EnvironmentFile=):
#   IMAGE_TAR_URL   e.g. https://github.com/.../releases/latest/download/opencode-proxy.tar
#   IMAGE_REF       e.g. docker.io/library/opencode-proxy:latest
#   SERVICE_NAME    e.g. opencode-proxy or opencode-proxy-local
#   STATE_DIR       default /etc/opencode-proxy
#
# No `sudo` calls in here: this always runs as root already, via the
# opencode-proxy-update.service systemd unit (no User= set, so systemd
# runs it as root), same as the ctr/systemctl commands it wraps require.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: run as root (this is normally invoked by opencode-proxy-update.service)" >&2
  exit 1
fi

: "${IMAGE_TAR_URL:?IMAGE_TAR_URL must be set}"
: "${IMAGE_REF:?IMAGE_REF must be set}"
: "${SERVICE_NAME:?SERVICE_NAME must be set}"
STATE_DIR="${STATE_DIR:-/etc/opencode-proxy}"

CURRENT_SHA_FILE="$STATE_DIR/current.sha256"
ROLLBACK_REF="${IMAGE_REF}-rollback"
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

ctr images tag "$IMAGE_REF" "$ROLLBACK_REF" >/dev/null 2>&1 || true # no-op on first-ever update

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
