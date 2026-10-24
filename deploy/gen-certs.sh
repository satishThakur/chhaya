#!/usr/bin/env bash
# Generate the CA, the gateway's server certificate and Hermes' client
# certificate. Run once; the output is gitignored (*.crt, *.key, certs/).
#
#   deploy/gen-certs.sh deploy/certs
#
# The CA is ours and signs exactly two certificates. Hermes presents its
# client cert or it does not reach the gateway at all.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCKERFILE="$HERE/Dockerfile.gateway"

die() { echo "error: $1" >&2; exit 1; }

# The uid the gateway container runs as, read out of Dockerfile.gateway rather
# than written down twice. These two drifting apart is the exact bug this
# function exists to prevent: the Dockerfile said 10001, this script left the
# key owned by the host user at 0600, and the gateway crash-looped on
# "load server keypair: permission denied".
gateway_uid() {
  local uid
  uid="$(sed -n 's/^RUN adduser .*-u \([0-9][0-9]*\).*/\1/p' "$DOCKERFILE" | head -1)"
  [ -n "$uid" ] || die "could not read the gateway uid from $DOCKERFILE — did the adduser line change?"
  printf '%s' "$uid"
}

# Grant one uid read access to a 0600 file.
#
# The gateway reads its key through a read-only bind mount, and bind mounts do
# not remap ownership: the key arrives inside the container owned by whoever ran
# this script, with no bits for anyone else. Nothing inside the container can fix
# that — :ro forbids it and there is no root phase to do it in — so the grant has
# to happen here, at generation time.
#
# An ACL rather than chmod 0644 because this is a private key, and rather than
# chgrp because that needs root; the file's owner can set an ACL on it unprivileged.
grant_read() {
  local file="$1" uid="$2"
  command -v setfacl >/dev/null 2>&1 || die "$(cat <<EOF
setfacl not found, but $file must be readable by uid $uid (the gateway container).
  Install it:  sudo apt install acl   (or: apk add acl)
  Or grant access without ACLs:  sudo chgrp $uid $file && chmod 640 $file
EOF
)"
  setfacl -m "u:${uid}:r" "$file" || die "setfacl failed on $file"
  # Prove it rather than assume it. A filesystem mounted without ACL support can
  # accept the call and silently not apply it, which would put us right back in
  # the crash loop with a script that claimed success.
  getfacl -p "$file" 2>/dev/null | grep -qE "^user:${uid}:r" \
    || die "the ACL did not stick on $file — does this filesystem support ACLs?"
}

# Reading a file also needs traverse (+x) on every directory above it. `mkdir -p`
# honours the caller's umask, so a umask of 077 produces a 0700 certs directory
# and the gateway fails with exactly the same "permission denied" even though the
# file ACL is correct. Granting +x to one uid keeps the directory shut to everyone
# else, which chmod 755 would not.
grant_traverse() {
  local dir="$1" uid="$2"
  setfacl -m "u:${uid}:x" "$dir" || die "setfacl failed on $dir"
  getfacl -p "$dir" 2>/dev/null | grep -qE "^user:${uid}:..x" \
    || die "the traverse ACL did not stick on $dir"
}

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

# Only gateway.key needs the grant. ca.key never leaves the host, and hermes.key
# is read by a container that currently runs as root — if that ever stops being
# true, Hermes will fail exactly the way the gateway did, and the fix is one more
# grant_read line here.
GATEWAY_UID="$(gateway_uid)"
grant_traverse . "$GATEWAY_UID"
grant_read gateway.key "$GATEWAY_UID"

echo "wrote CA, gateway and hermes certs to $PWD"
echo "granted uid $GATEWAY_UID traverse on $PWD and read on gateway.key"
