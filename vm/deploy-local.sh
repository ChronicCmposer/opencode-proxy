#!/usr/bin/env bash
# Assumes the VM and opencode (on 127.0.0.1:4096) are already up — this
# does not provision the VM or install opencode, and must be run from a
# checkout of this repo (reaches into ../scripts and ../systemd).
#
# Usage (as root):
#   vm/deploy-local.sh <path-to-opencode-proxy.tar> <cert-dir> <remote-wss-url> [opencode-url] [image-tar-url]
#
# <path-to-opencode-proxy.tar> is a local file for this initial import
# (works offline / from a shared folder); [image-tar-url] is the separate
# URL the 6-hourly updater polls afterward.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: run as root (sudo $0 ...)" >&2
  exit 1
fi
if [[ $# -lt 3 ]]; then
  echo "usage: $0 <path-to-opencode-proxy.tar> <cert-dir> <remote-wss-url> [opencode-url] [image-tar-url]" >&2
  exit 1
fi

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

image_tar="$1"
cert_dir="$2"
remote_url="$3"
opencode_url="${4:-http://127.0.0.1:4096}"
image_tar_url="${5:-https://github.com/ChronicCmposer/opencode-proxy/releases/latest/download/opencode-proxy.tar}"
image_ref="docker.io/library/opencode-proxy:latest" # must match Makefile's IMAGE_NAME

for f in ca.crt tunnel.crt tunnel.key; do
  if [[ ! -f "$cert_dir/$f" ]]; then
    echo "error: $cert_dir/$f not found — copy the certs from pki/issue-tunnel.sh first" >&2
    exit 1
  fi
done
if [[ ! -f "$image_tar" ]]; then
  echo "error: $image_tar not found" >&2
  exit 1
fi

if ! command -v ctr >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "containerd/ctr or curl not found — installing..."
  apt-get update
  apt-get install -y containerd curl
fi
systemctl enable --now containerd

mkdir -p /etc/opencode-proxy
install -m 644 "$cert_dir/ca.crt" /etc/opencode-proxy/ca.crt
install -m 644 "$cert_dir/tunnel.crt" /etc/opencode-proxy/tunnel.crt
install -m 600 "$cert_dir/tunnel.key" /etc/opencode-proxy/tunnel.key

echo "importing $image_tar into containerd..."
ctr images import "$image_tar"
sha256sum "$image_tar" | awk '{print $1}' > /etc/opencode-proxy/current.sha256

cat > /etc/systemd/system/opencode-proxy-local.service <<UNIT
[Unit]
Description=opencode-proxy local (containerd)
After=network-online.target containerd.service
Wants=network-online.target
Requires=containerd.service

[Service]
# ctr run keeps the earlier container/task names around after a stop or
# crash; clear them before each start so Restart=always doesn't fail with
# "already exists".
ExecStartPre=-/usr/bin/ctr task kill opencode-proxy-local
ExecStartPre=-/usr/bin/ctr task rm opencode-proxy-local
ExecStartPre=-/usr/bin/ctr container rm opencode-proxy-local
ExecStart=/usr/bin/ctr run --rm --net-host \\
  --mount type=bind,src=/etc/opencode-proxy,dst=/etc/opencode-proxy,options=rbind:ro \\
  ${image_ref} opencode-proxy-local \\
  --local --remote-url ${remote_url} \\
  --opencode-url ${opencode_url} \\
  --ca /etc/opencode-proxy/ca.crt \\
  --cert /etc/opencode-proxy/tunnel.crt \\
  --key /etc/opencode-proxy/tunnel.key
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now opencode-proxy-local

install -m 755 "$repo_dir/scripts/update-image.sh" /usr/local/bin/opencode-proxy-update.sh

cat > /etc/opencode-proxy/update.env <<ENV
IMAGE_TAR_URL=$image_tar_url
IMAGE_REF=$image_ref
SERVICE_NAME=opencode-proxy-local
STATE_DIR=/etc/opencode-proxy
ENV

install -m 644 "$repo_dir/systemd/opencode-proxy-update.service" /etc/systemd/system/opencode-proxy-update.service
install -m 644 "$repo_dir/systemd/opencode-proxy-update.timer" /etc/systemd/system/opencode-proxy-update.timer

systemctl daemon-reload
systemctl enable --now opencode-proxy-update.timer

echo "done. check status with: systemctl status opencode-proxy-local"
echo "and logs with:           journalctl -u opencode-proxy-local -f"
echo "update timer:            systemctl list-timers opencode-proxy-update.timer"
