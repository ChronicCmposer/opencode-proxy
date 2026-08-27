#!/usr/bin/env bash
# Assumes the VM and opencode (on 127.0.0.1:4096) are already up — this
# does not provision the VM or install opencode.
#
# Usage (as root):
#   vm/deploy-local.sh <path-to-opencode-proxy.tar> <cert-dir> <remote-wss-url> [opencode-url] [image-tar-url] [repo-ref]
#
# <path-to-opencode-proxy.tar> is a local file for this initial import
# (works offline / from a shared folder); [image-tar-url] is the separate
# URL the 6-hourly updater polls afterward. [repo-ref] pins the tag/branch/
# commit this script fetches its own systemd units and update script from
# (see "fetch supporting files" below) — defaults to main.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "error: run as root (sudo $0 ...)" >&2
  exit 1
fi
if [[ $# -lt 3 ]]; then
  echo "usage: $0 <path-to-opencode-proxy.tar> <cert-dir> <remote-wss-url> [opencode-url] [image-tar-url] [repo-ref]" >&2
  exit 1
fi

image_tar="$1"
cert_dir="$2"
remote_url="$3"
opencode_url="${4:-http://127.0.0.1:4096}"
image_tar_url="${5:-https://github.com/ChronicCmposer/opencode-proxy/releases/latest/download/opencode-proxy.tar}"
repo_ref="${6:-main}"
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

if ! command -v ctr >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1 || ! command -v tar >/dev/null 2>&1 || ! command -v openssl >/dev/null 2>&1; then
  echo "containerd/ctr, curl, tar, or openssl not found — installing..."
  apt-get update
  apt-get install -y containerd curl tar openssl
fi
systemctl enable --now containerd

mkdir -p /etc/opencode-proxy
install -m 644 "$cert_dir/ca.crt" /etc/opencode-proxy/ca.crt
install -m 644 "$cert_dir/tunnel.crt" /etc/opencode-proxy/tunnel.crt
install -m 600 "$cert_dir/tunnel.key" /etc/opencode-proxy/tunnel.key

# Fetch this ref's systemd units, update script, and the release signing
# public key as a tarball rather than assuming a repo checkout is available —
# a plain codeload fetch, extract just what's needed, then discard the rest.
# Done before the import so the signing key is available to verify the image.
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
echo "fetching supporting files from ref $repo_ref..."
curl -fsSL -o "$work_dir/repo.tar.gz" \
  "https://codeload.github.com/ChronicCmposer/opencode-proxy/tar.gz/${repo_ref}"
mkdir -p "$work_dir/src"
tar -xzf "$work_dir/repo.tar.gz" -C "$work_dir/src" --strip-components=1

# Release signing public key — used to verify the recurring image updates
# (update-image.sh) and, if a signature sits beside the bootstrap tar, this
# initial import too.
install -m 644 "$work_dir/src/scripts/release-signing.pub" /etc/opencode-proxy/release-signing.pub

echo "importing $image_tar into containerd..."
# The bootstrap tar is a local, operator-supplied file (works offline). If a
# detached signature sits beside it, verify it; otherwise proceed but say so —
# the recurring auto-updates over the network are always signature-verified.
if [[ -f "$image_tar.sig" ]]; then
  openssl pkeyutl -verify -rawin -pubin -inkey /etc/opencode-proxy/release-signing.pub \
    -in "$image_tar" -sigfile "$image_tar.sig"
  echo "bootstrap image signature verified"
else
  echo "warning: no $image_tar.sig beside the bootstrap tar — importing it unverified (trusting this local file); auto-updates will still be signature-verified" >&2
fi
ctr images import "$image_tar"
sha256sum "$image_tar" | awk '{print $1}' > /etc/opencode-proxy/current.sha256

cat > /etc/opencode-proxy/local.env <<ENV
IMAGE_REF=$image_ref
REMOTE_URL=$remote_url
OPENCODE_URL=$opencode_url
ENV

# Shipped from the repo rather than written inline so the defaults have one
# source of truth. Every key is optional — delete one and the binary's built-in
# default applies.
install -m 644 "$work_dir/src/config.example.json" /etc/opencode-proxy/config.json

install -m 644 "$work_dir/src/systemd/opencode-proxy-local.service" /etc/systemd/system/opencode-proxy-local.service

systemctl daemon-reload
systemctl enable --now opencode-proxy-local

install -m 755 "$work_dir/src/scripts/update-image.sh" /usr/local/bin/opencode-proxy-update.sh

cat > /etc/opencode-proxy/update.env <<ENV
IMAGE_TAR_URL=$image_tar_url
IMAGE_REF=$image_ref
SERVICE_NAME=opencode-proxy-local
STATE_DIR=/etc/opencode-proxy
ENV

install -m 644 "$work_dir/src/systemd/opencode-proxy-update.service" /etc/systemd/system/opencode-proxy-update.service
install -m 644 "$work_dir/src/systemd/opencode-proxy-update.timer" /etc/systemd/system/opencode-proxy-update.timer

systemctl daemon-reload
systemctl enable --now opencode-proxy-update.timer

echo "done. check status with: systemctl status opencode-proxy-local"
echo "and logs with:           journalctl -u opencode-proxy-local -f"
echo "update timer:            systemctl list-timers opencode-proxy-update.timer"
