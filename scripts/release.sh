#!/usr/bin/env bash
# Builds and publishes a GitHub Release for the exact commit HEAD is on.
# Run via `make release`, after `make bump-version LEVEL=...` has tagged
# and pushed the commit you want to release.
#
# Refuses to run unless: the tree is clean, local HEAD matches the remote
# branch HEAD, HEAD is exactly tagged, and that tag exists on origin. This
# is deliberately strict — a release should always be reproducible from a
# tag someone else can check out and get the identical build.
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

echo "releasing $tag..."
make image

gh release create "$tag" opencode-proxy.tar opencode-proxy.tar.sha256 \
  --title "$tag" --generate-notes

echo "released $tag"
