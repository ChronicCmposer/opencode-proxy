#!/usr/bin/env bash
# Certificates must already be uploaded to SSM (see upload-certs.sh) before
# the instance's UserData can fetch them at boot.
#
# Usage:
#   cloudformation/deploy.sh <stack-name> <domain-name> <key-pair-name> <admin-cidr> <repo-ref>
#
# <repo-ref> must be an immutable ref — a release tag (vX.Y.Z) or a full
# 40-char commit SHA, never a branch: the instance fetches root-run scripts
# and unit files from it at boot, and the same ref must not repoint under a
# deployed stack.
#
# Example:
#   cloudformation/deploy.sh opencode-proxy code.example.com my-ec2-key 203.0.113.9/32 v1.2.0
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [[ $# -ne 5 ]]; then
  echo "usage: $0 <stack-name> <domain-name> <key-pair-name> <admin-cidr> <repo-ref>" >&2
  exit 1
fi
stack_name="$1" domain="$2" key_name="$3" admin_cidr="$4" repo_ref="$5"

# The release signing public key is passed in as a template parameter — an
# immutable, out-of-band trust anchor — rather than fetched from the repo
# tarball at boot, so the key verifying the image can't be swapped by whoever
# supplies the image. See stack.yaml's ReleaseSigningKey.
signing_key="$(cat ../scripts/release-signing.pub)"

aws cloudformation deploy \
  --stack-name "$stack_name" \
  --template-file stack.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      DomainName="$domain" \
      KeyName="$key_name" \
      AdminCidr="$admin_cidr" \
      RepoRef="$repo_ref" \
      ReleaseSigningKey="$signing_key"

aws cloudformation describe-stacks --stack-name "$stack_name" \
  --query "Stacks[0].Outputs" --output table
