#!/usr/bin/env bash
# The very first tag must be created manually — there's nothing to bump
# from yet: git tag v0.1.0 && git push origin v0.1.0
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

level="${1:-}"
case "$level" in
  major|minor|patch) ;;
  *)
    echo "usage: $0 major|minor|patch  (or: make bump-version LEVEL=patch)" >&2
    exit 1
    ;;
esac

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

# Not `git describe --abbrev=0`: when multiple tags point at the same
# commit (as happens here, since bumping doesn't create a new commit),
# it picks unpredictably rather than the highest version — confirmed by
# testing repeated bumps against a disposable git remote.
latest_tag="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)"
if [[ -z "$latest_tag" ]]; then
  echo "error: no tag found to bump from — create the first one manually:" >&2
  echo "  git tag v0.1.0 && git push origin v0.1.0" >&2
  exit 1
fi

if [[ ! "$latest_tag" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "error: latest tag '$latest_tag' isn't a plain vMAJOR.MINOR.PATCH tag — bump it manually" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

case "$level" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
new_tag="v$major.$minor.$patch"

echo "bumping $latest_tag -> $new_tag ($level)"
git tag "$new_tag"
git push origin "$new_tag"
echo "tagged and pushed $new_tag — run 'make release' to build and publish it"
