#!/bin/bash
# First boot only. Installs what the stack needs and clones the (public) repo.
# It does NOT start the stack: that happens only after secrets and data are in place.
# Log: /var/log/chhaya-bootstrap.log. Done marker: /var/lib/chhaya-bootstrap.done
set -euxo pipefail
exec > >(tee -a /var/log/chhaya-bootstrap.log) 2>&1

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y docker.io docker-compose-v2 git make curl

# Go (the bot check runs `chhayactl botcheck` on the host; go.mod wants 1.25+) and
# the AWS CLI are snaps: Ubuntu 24.04's apt has neither at a usable version.
snap install go --classic --channel=1.25/stable
snap install aws-cli --classic --channel=v2/stable

systemctl enable --now docker
usermod -aG docker ubuntu

sudo -u ubuntu git clone ${repo_url} /home/ubuntu/chhaya

touch /var/lib/chhaya-bootstrap.done
