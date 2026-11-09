#!/usr/bin/env bash
# Generate the CA, the gateway's server certificate and Hermes' client
# certificate. Run once; the output is gitignored (*.crt, *.key, certs/).
#
#   deploy/gen-certs.sh deploy/certs
#
# The CA is ours and signs exactly two certificates. Hermes presents its
# client cert or it does not reach the gateway at all.
set -euo pipefail

die() { echo "error: $1" >&2; exit 1; }

OUT="${1:-deploy/certs}"
mkdir -p "$OUT"
cd "$OUT"

if [ -f ca.key ]; then
  echo "certs already exist in $PWD — delete them first if you really mean to rotate"
  exit 1
fi

# basicConstraints and keyUsage are not optional decoration. Without them Go's
# crypto/tls and curl still accept this CA — which is why `make dev-status` passed
# for weeks — but Python's stricter verifier refuses it outright with
# `CERTIFICATE_VERIFY_FAILED: CA cert does not include key usage extension`, and
# Hermes verifies in Python. A CA that only some verifiers accept is a CA that
# works until the one client that matters tries to use it.
openssl req -x509 -newkey rsa:4096 -days 3650 -nodes \
  -keyout ca.key -out ca.crt -subj "/CN=chhaya-ca" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

# Server. SAN must cover whatever Hermes puts in its mcp_servers url.
openssl req -newkey rsa:4096 -nodes -keyout gateway.key -out gateway.csr \
  -subj "/CN=chhaya-gateway"
openssl x509 -req -in gateway.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out gateway.crt -days 825 \
  -extfile <(printf "subjectAltName=DNS:gateway,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment")

# Client, for Hermes.
openssl req -newkey rsa:4096 -nodes -keyout hermes.key -out hermes.csr \
  -subj "/CN=hermes"
openssl x509 -req -in hermes.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out hermes.crt -days 825 \
  -extfile <(printf "extendedKeyUsage=clientAuth\nkeyUsage=critical,digitalSignature")

rm -f ./*.csr
chmod 600 ./*.key

# No ACLs, no chgrp. Both containers run as the operator's uid: the gateway through
# `user:` in docker-compose.yml, Hermes through HERMES_UID, which the image's s6 stage
# remaps its `hermes` user to. A 0600 key owned by whoever ran this script is therefore
# readable by exactly the process that needs it (bind mounts do not remap ownership, so
# the container sees the host's owner). Earlier versions granted uids 10001 and 10000
# with setfacl; that was needed only while the containers ran as those uids, and it
# made certificate generation fail on any host without the `acl` package.
echo "wrote CA, gateway and hermes certs to $PWD (keys are 0600, owned by uid $(id -u))"
