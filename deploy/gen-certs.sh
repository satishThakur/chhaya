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

# Both containers need to read their own key, and neither runs as the user who
# generated it.
#
# An earlier version of this comment claimed hermes.key needed no grant because the
# Hermes container runs as root. That is true of the image's `User` and false of the
# process: s6 starts the agent with `s6-setuidgid hermes` (uid 10000, from the pinned
# image's /etc/passwd). The visible symptom was not "permission denied" — it was
# Hermes failing its MCP connection, blowing its 60 s startup deadline and
# crash-looping, because a client key it cannot read is a TLS handshake it cannot
# finish. Only ca.key is read by nobody but the host.
GATEWAY_UID="$(gateway_uid)"
HERMES_UID=10000

grant_traverse . "$GATEWAY_UID"
grant_traverse . "$HERMES_UID"
grant_read gateway.key "$GATEWAY_UID"
grant_read hermes.key "$HERMES_UID"

echo "wrote CA, gateway and hermes certs to $PWD"
echo "granted uid $GATEWAY_UID read on gateway.key, uid $HERMES_UID read on hermes.key"
