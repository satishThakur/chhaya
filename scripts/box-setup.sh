#!/usr/bin/env bash
# Sets up (or repairs) the EC2 box: the packages, snaps, Docker access and repo clone the
# stack needs. Idempotent: run it on a fresh box (infra/terraform/user_data.sh does) and run
# it again on a live one after a `git pull`, so the box cannot drift from what the repo says
# it needs. It never starts the stack and never touches ~/.chhaya or any env file.
#
#   sudo scripts/box-setup.sh           install whatever is missing
#   sudo scripts/box-setup.sh --cron    ... and install deploy/crontab.box for the box user
#   scripts/box-setup.sh --check        change nothing; list what is missing (no root needed;
#                                       add --cron to include the crontab)
#
# Environment: CHHAYA_BOX_USER (default ubuntu), CHHAYA_REPO_URL (default the public repo),
# CHHAYA_REPO_DIR (default /home/<user>/chhaya). Ubuntu 24.04 only.
set -euo pipefail

BOX_USER="${CHHAYA_BOX_USER:-ubuntu}"
REPO_URL="${CHHAYA_REPO_URL:-https://github.com/satishthakur/chhaya.git}"
REPO_DIR="${CHHAYA_REPO_DIR:-/home/$BOX_USER/chhaya}"
GO_CHANNEL="1.25/stable"            # go.mod wants 1.25+; the bot check runs `go run` on the host
APT_PACKAGES=(docker.io docker-compose-v2 git make curl sqlite3 cron)
SNAPS=("go --classic --channel=$GO_CHANNEL" "aws-cli --classic --channel=v2/stable")

mode=install; want_cron=0
for a in "$@"; do
  case "$a" in
    --check) mode=check ;;
    --cron)  want_cron=1 ;;
    *) echo "usage: $0 [--check] [--cron]" >&2; exit 2 ;;
  esac
done

missing=()
say() { printf '%s\n' "$*"; }

# --- apt packages ------------------------------------------------------------
need_apt=()
for p in "${APT_PACKAGES[@]}"; do dpkg -s "$p" >/dev/null 2>&1 || need_apt+=("$p"); done
if [ "${#need_apt[@]}" -gt 0 ]; then
  missing+=("apt: ${need_apt[*]}")
  if [ "$mode" = install ]; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y "${need_apt[@]}"
  fi
fi

# --- snaps (Ubuntu 24.04's apt has neither at a usable version) ---------------
for spec in "${SNAPS[@]}"; do
  name="${spec%% *}"
  if ! snap list "$name" >/dev/null 2>&1; then
    missing+=("snap: $name")
    # shellcheck disable=SC2086
    [ "$mode" = install ] && snap install $spec
  fi
done

# --- docker running and usable by the box user --------------------------------
if ! systemctl is-active --quiet docker; then
  missing+=("docker service not running")
  [ "$mode" = install ] && systemctl enable --now docker
fi
if ! id -nG "$BOX_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
  missing+=("$BOX_USER is not in the docker group")
  [ "$mode" = install ] && usermod -aG docker "$BOX_USER"
fi

# --- the repo ------------------------------------------------------------------
if [ ! -d "$REPO_DIR/.git" ]; then
  missing+=("repo clone at $REPO_DIR")
  [ "$mode" = install ] && sudo -u "$BOX_USER" git clone "$REPO_URL" "$REPO_DIR"
fi

# --- cron entry (only when asked: on a fresh box there is no data to back up yet) -
if [ "$want_cron" -eq 1 ] && ! crontab -u "$BOX_USER" -l 2>/dev/null | grep -q 'dev.sh backup'; then
  missing+=("nightly backup crontab for $BOX_USER")
  [ "$mode" = install ] && crontab -u "$BOX_USER" "$REPO_DIR/deploy/crontab.box"
fi

if [ "${#missing[@]}" -eq 0 ]; then
  say "box is set up: nothing missing"
elif [ "$mode" = check ]; then
  say "MISSING:"; printf '  - %s\n' "${missing[@]}"
  exit 1
else
  say "installed:"; printf '  - %s\n' "${missing[@]}"
fi
