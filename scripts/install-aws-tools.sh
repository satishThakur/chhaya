#!/usr/bin/env bash
# One-time setup of the tools the deployment needs on this (Ubuntu, amd64) machine.
# Run one step at a time:
#
#   scripts/install-aws-tools.sh terraform
#   scripts/install-aws-tools.sh awscli
#   scripts/install-aws-tools.sh ssm-plugin
#   scripts/install-aws-tools.sh check
#
# Each step uses sudo only for the install itself. Terraform comes from
# releases.hashicorp.com with its SHA256 checked (HashiCorp's apt repo may not
# carry newer Ubuntu codenames); the AWS CLI v2 is AWS's own snap; the Session
# Manager plugin is AWS's own .deb.
set -euo pipefail

die() { printf 'error: %s\n' "$1" >&2; exit 1; }
[ "$(dpkg --print-architecture)" = amd64 ] || die "this script assumes amd64"

install_terraform() {
  local v="${TERRAFORM_VERSION:-}"
  [ -n "$v" ] || v="$(curl -fsSL https://checkpoint-api.hashicorp.com/v1/check/terraform | sed -n 's/.*"current_version":"\([^"]*\)".*/\1/p')"
  [ -n "$v" ] || die "could not work out the latest Terraform version; set TERRAFORM_VERSION"
  command -v unzip >/dev/null || die "unzip is missing: sudo apt-get install -y unzip"

  local d; d="$(mktemp -d)"; trap 'rm -rf "$d"' RETURN
  local base="https://releases.hashicorp.com/terraform/$v" zip="terraform_${v}_linux_amd64.zip"
  echo "downloading Terraform $v"
  curl -fsSL -o "$d/$zip"    "$base/$zip"
  curl -fsSL -o "$d/SHA256SUMS" "$base/terraform_${v}_SHA256SUMS"
  (cd "$d" && grep " $zip\$" SHA256SUMS | sha256sum -c -) || die "checksum mismatch; not installing"
  unzip -q -o "$d/$zip" terraform -d "$d"
  sudo install -m 0755 "$d/terraform" /usr/local/bin/terraform
  terraform version | head -1
}

install_awscli() {
  # v2/stable explicitly: the default snap channel is still AWS CLI v1.
  sudo snap install aws-cli --classic --channel=v2/stable
  aws --version
}

install_ssm_plugin() {
  local d; d="$(mktemp -d)"; trap 'rm -rf "$d"' RETURN
  curl -fsSL -o "$d/ssm.deb" https://s3.amazonaws.com/session-manager-downloads/plugin/latest/ubuntu_64bit/session-manager-plugin.deb
  sudo dpkg -i "$d/ssm.deb"
  session-manager-plugin --version
}

check() {
  local bad=0
  for c in terraform aws session-manager-plugin; do
    if command -v "$c" >/dev/null; then printf 'ok      %s\n' "$c"; else printf 'MISSING %s\n' "$c"; bad=1; fi
  done
  exit "$bad"
}

case "${1:-}" in
  terraform)  install_terraform ;;
  awscli)     install_awscli ;;
  ssm-plugin) install_ssm_plugin ;;
  check)      check ;;
  *) echo "usage: $0 terraform | awscli | ssm-plugin | check" >&2; exit 2 ;;
esac
