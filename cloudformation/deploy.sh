#!/usr/bin/env bash
# Certificates must already be uploaded to SSM (see upload-certs.sh) before
# the instance's UserData can fetch them at boot.
#
# Usage:
#   cloudformation/deploy.sh <stack-name> <domain-name> <key-pair-name> <admin-cidr> <repo-ref> [tunnel-cn] [instance-type]
#
# The optional [instance-type] overrides the InstanceType template parameter
# (default t4g.nano); changing it forces an instance replacement, which can be
# used to pick up new boot material.
# <repo-ref> is anything git can resolve in your local checkout — a release
# tag (vX.Y.Z), a branch, or a commit SHA. It is resolved here, on your
# trusted machine, to the full commit SHA it currently names, and that SHA is
# what the stack pins: the instance fetches root-run scripts and unit files
# from it at boot, and a commit SHA is content-addressed, so unlike a tag
# (which can be force-moved on GitHub) it can't be repointed under a deployed
# stack. Resolve, don't trust the name: check `git log` for the SHA printed
# below before confirming a production deploy.
#
# Example:
#   cloudformation/deploy.sh opencode-proxy code.example.com my-ec2-key 203.0.113.9/32 v1.2.0 t4g.micro
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [[ $# -lt 5 || $# -gt 7 ]]; then
  echo "usage: $0 <stack-name> <domain-name> <key-pair-name> <admin-cidr> <repo-ref> [tunnel-cn] [instance-type]" >&2
  exit 1
fi
stack_name="$1" domain="$2" key_name="$3" admin_cidr="$4" repo_ref="$5"
# The CN of the tunnel certificate the home Mac uses (pki/issue-tunnel.sh's
# default is "home-mac"); the remote pins the tunnel upgrade to it. Override
# only if you issued the tunnel cert with a different CN.
tunnel_cn="${6:-home-mac}"
instance_type="${7:-}"

# Resolve the ref to the full 40-char commit SHA it points at right now. The
# stack pins that SHA (RepoRef's AllowedPattern rejects anything else), so what
# runs as root at boot is fixed to a commit you can review, not a mutable name.
repo_sha="$(git -C .. rev-parse --verify --quiet "${repo_ref}^{commit}" || true)"
if [[ ! "$repo_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: could not resolve '$repo_ref' to a commit SHA in this checkout" >&2
  echo "       pass a tag, branch, or SHA that 'git rev-parse' can resolve here" >&2
  exit 1
fi
echo "pinning RepoRef to $repo_sha (resolved from '$repo_ref')"

# The release signing public key is passed in as a template parameter — an
# immutable, out-of-band trust anchor — rather than fetched from the repo
# tarball at boot, so the key verifying the image can't be swapped by whoever
# supplies the image. See stack.yaml's ReleaseSigningKey.
signing_key="$(cat ../scripts/release-signing.pub)"

param_overrides=(
  "DomainName=$domain"
  "TunnelCN=$tunnel_cn"
  "KeyName=$key_name"
  "AdminCidr=$admin_cidr"
  "RepoRef=$repo_sha"
  "ReleaseSigningKey=$signing_key"
)
if [[ -n "$instance_type" ]]; then
  param_overrides+=("InstanceType=$instance_type")
fi

aws cloudformation deploy \
  --stack-name "$stack_name" \
  --template-file stack.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides "${param_overrides[@]}"

aws cloudformation describe-stacks --stack-name "$stack_name" \
  --query "Stacks[0].Outputs" --output table
