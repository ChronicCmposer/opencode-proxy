#!/usr/bin/env bash
# Refuses to run unless HEAD is exactly, cleanly, pushed-tagged — a release
# should always be reproducible from a tag someone else can check out.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: uncommitted changes — commit or stash them first" >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch origin "$branch" --quiet
local_head="$(git rev-parse HEAD)"
remote_head="$(git rev-parse "origin/$branch")"
if [[ "$local_head" != "$remote_head" ]]; then
  echo "error: local HEAD ($local_head) differs from origin/$branch ($remote_head) — push or pull first" >&2
  exit 1
fi

tag="$(git describe --tags --exact-match HEAD 2>/dev/null || true)"
if [[ -z "$tag" ]]; then
  echo "error: HEAD is not exactly tagged — run 'make bump-version LEVEL=patch|minor|major' first" >&2
  exit 1
fi

if [[ -z "$(git ls-remote --tags origin "refs/tags/$tag")" ]]; then
  echo "error: tag $tag exists locally but not on origin — push it first (git push origin $tag)" >&2
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh (GitHub CLI) not found — install/authenticate it to publish releases" >&2
  exit 1
fi

PRIV="${RELEASE_SIGNING_DIR:-signing}/release.key"
if [[ ! -f "$PRIV" ]]; then
  echo "error: signing key $PRIV not found — run scripts/init-signing-key.sh first" >&2
  echo "releases must be signed; hosts verify the signature before importing the image (see update-image.sh)" >&2
  exit 1
fi

echo "releasing $tag..."
make image

# Sign the image tar so every host can verify authenticity — not just
# integrity — before running it as root. The detached signature is published
# alongside the tar and its checksum.
openssl pkeyutl -sign -rawin -inkey "$PRIV" \
  -in opencode-proxy.tar -out opencode-proxy.tar.sig
echo "signed opencode-proxy.tar -> opencode-proxy.tar.sig"

gh release create "$tag" \
  opencode-proxy.tar opencode-proxy.tar.sha256 opencode-proxy.tar.sig \
  --title "$tag" --generate-notes

echo "released $tag"
