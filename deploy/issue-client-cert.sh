#!/usr/bin/env bash
# Issue one client certificate from the existing CA, for a gateway client other than Hermes.
#
#   deploy/issue-client-cert.sh dreamer deploy/certs
#
# The certificate's common name is the actor name the gateway puts in the audit log and the name
# internal/policy knows the client by: an actor policy does not know is refused. Today the only
# names that mean anything are `hermes` and `dreamer`. The key is 0600 and belongs to the user who
# ran this; it is mounted into no container.
set -euo pipefail

NAME="${1:?usage: issue-client-cert.sh NAME [DIR]}"
OUT="${2:-deploy/certs}"
case "$NAME" in *[!a-z0-9_-]*|"") echo "error: NAME must be lowercase letters, digits, - or _" >&2; exit 1;; esac
cd "$OUT"
[ -f ca.key ] && [ -f ca.crt ] || { echo "error: no CA in $PWD; run deploy/gen-certs.sh first" >&2; exit 1; }
if [ -f "$NAME.key" ]; then echo "$NAME.key already exists in $PWD; delete it first to re-issue"; exit 0; fi

openssl req -newkey rsa:4096 -nodes -keyout "$NAME.key" -out "$NAME.csr" -subj "/CN=$NAME"
openssl x509 -req -in "$NAME.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out "$NAME.crt" -days 825 \
  -extfile <(printf "extendedKeyUsage=clientAuth\nkeyUsage=critical,digitalSignature")
rm -f "$NAME.csr"
chmod 600 "$NAME.key"
echo "issued $NAME.crt and $NAME.key in $PWD"
