#!/bin/bash
# First boot only (Terraform ignores later edits to this on purpose: replacing the
# instance would take its data with it). It installs git, clones the public repo and runs
# scripts/box-setup.sh, which holds the real list of what the box needs and is safe to
# re-run on a live box. It does NOT start the stack: that happens only after secrets and
# data are in place. It does NOT install the backup cron: there is no data to back up yet.
# Log: /var/log/chhaya-bootstrap.log. Done marker: /var/lib/chhaya-bootstrap.done
set -euxo pipefail
exec > >(tee -a /var/log/chhaya-bootstrap.log) 2>&1

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y git

sudo -u ubuntu git clone ${repo_url} /home/ubuntu/chhaya
CHHAYA_REPO_URL=${repo_url} bash /home/ubuntu/chhaya/scripts/box-setup.sh

touch /var/lib/chhaya-bootstrap.done
