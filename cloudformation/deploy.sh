#!/usr/bin/env bash
# Certificates must already be uploaded to SSM (see upload-certs.sh) before
# the instance's UserData can fetch them at boot.
#
# Usage:
#   cloudformation/deploy.sh <stack-name> <domain-name> <key-pair-name> <admin-cidr>
#
# Example:
#   cloudformation/deploy.sh opencode-proxy code.example.com my-ec2-key 203.0.113.9/32
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if [[ $# -ne 4 ]]; then
  echo "usage: $0 <stack-name> <domain-name> <key-pair-name> <admin-cidr>" >&2
  exit 1
fi
stack_name="$1" domain="$2" key_name="$3" admin_cidr="$4"

aws cloudformation deploy \
  --stack-name "$stack_name" \
  --template-file stack.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      DomainName="$domain" \
      KeyName="$key_name" \
      AdminCidr="$admin_cidr"

aws cloudformation describe-stacks --stack-name "$stack_name" \
  --query "Stacks[0].Outputs" --output table
