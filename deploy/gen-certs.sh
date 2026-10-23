#!/usr/bin/env bash
# Generate the CA, the gateway's server certificate and Hermes' client
# certificate. Run once; the output is gitignored (*.crt, *.key, certs/).
#
#   deploy/gen-certs.sh deploy/certs
#
# The CA is ours and signs exactly two certificates. Hermes presents its
# client cert or it does not reach the gateway at all.
set -euo pipefail

OUT="${1:-deploy/certs}"
mkdir -p "$OUT"
cd "$OUT"

if [ -f ca.key ]; then
  echo "certs already exist in $PWD — delete them first if you really mean to rotate"
  exit 1
fi

openssl req -x509 -newkey rsa:4096 -days 3650 -nodes \
  -keyout ca.key -out ca.crt -subj "/CN=chhaya-ca"

# Server. SAN must cover whatever Hermes puts in its mcp_servers url.
openssl req -newkey rsa:4096 -nodes -keyout gateway.key -out gateway.csr \
  -subj "/CN=chhaya-gateway"
openssl x509 -req -in gateway.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out gateway.crt -days 825 \
  -extfile <(printf "subjectAltName=DNS:gateway,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth")

# Client, for Hermes.
openssl req -newkey rsa:4096 -nodes -keyout hermes.key -out hermes.csr \
  -subj "/CN=hermes"
openssl x509 -req -in hermes.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out hermes.crt -days 825 \
  -extfile <(printf "extendedKeyUsage=clientAuth")

rm -f ./*.csr
chmod 600 ./*.key
echo "wrote CA, gateway and hermes certs to $PWD"
