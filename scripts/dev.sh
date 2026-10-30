#!/usr/bin/env bash
# Local development control for the two-container stack.
#
#   scripts/dev.sh up|down|restart|status|logs|audit|stop|resume|doctor
#
# Invoked through the Makefile (`make dev-up`), which is where the verbs are
# discoverable. The logic lives here because preflight needs real conditionals.
#
# The preflight exists because the two most expensive mistakes in this system are
# silent ones: starting with the wrong env file mounted (which crosses the two bots'
# tokens and makes the whole safety model decorative), and starting without the
# certificates the gateway needs (which looks like a network problem for ten minutes).
# Both fail here, before anything starts, naming the file to fix.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY="$ROOT/deploy"
COMPOSE=(docker compose -f "$DEPLOY/docker-compose.yml")
RENDERED="$DEPLOY/.hermes-config.rendered.yaml"

red()  { printf '\033[31m%s\033[0m\n' "$1"; }
grn()  { printf '\033[32m%s\033[0m\n' "$1"; }
ylw()  { printf '\033[33m%s\033[0m\n' "$1"; }
die()  { red "error: $1"; exit 1; }

# has_key FILE KEY -> true if KEY is present and non-empty
has_key() { grep -qE "^${2}=.+" "$1" 2>/dev/null; }
# mentions FILE KEY -> true if KEY appears at all, even empty
mentions() { grep -qE "^${2}=" "$1" 2>/dev/null; }

# The uid the gateway container runs as, read from the Dockerfile so this file
# and that one cannot drift. Empty if the adduser line ever changes shape.
gateway_uid() { sed -n 's/^RUN adduser .*-u \([0-9][0-9]*\).*/\1/p' "$DEPLOY/Dockerfile.gateway" 2>/dev/null | head -1; }

# The uid Hermes runs as. Not the image's `User` (root) — s6 starts the agent with
# `s6-setuidgid hermes`, uid 10000 from the pinned image's /etc/passwd. Kept in step
# with deploy/gen-certs.sh, which grants hermes.key to the same number.
HERMES_UID=10000

# readable_by FILE UID -> true if that uid can read the file, by any of the three
# routes that work: an ACL entry, a matching group with the group bit set, or
# world-readable. Bind mounts pass ownership through unremapped, so what the host
# says here is exactly what the container sees.
readable_by() {
  local f="$1" uid="$2" mode
  getfacl -p "$f" 2>/dev/null | grep -qE "^user:${uid}:r" && return 0
  mode="$(stat -c '%a' "$f" 2>/dev/null)" || return 1
  case "${mode: -1}" in 4|5|6|7) return 0 ;; esac
  if [ "$(stat -c '%g' "$f" 2>/dev/null)" = "$uid" ]; then
    case "${mode: -2:1}" in 4|5|6|7) return 0 ;; esac
  fi
  return 1
}

# traversable_by DIR UID -> true if that uid has +x on the directory. A correct
# ACL on the key is not enough on its own; without traverse the open fails the
# same way, which is a confusing half-hour if you are only looking at the file.
traversable_by() {
  local d="$1" uid="$2" mode
  getfacl -p "$d" 2>/dev/null | grep -qE "^user:${uid}:..x" && return 0
  mode="$(stat -c '%a' "$d" 2>/dev/null)" || return 1
  case "${mode: -1}" in 1|3|5|7) return 0 ;; esac
  if [ "$(stat -c '%g' "$d" 2>/dev/null)" = "$uid" ]; then
    case "${mode: -2:1}" in 1|3|5|7) return 0 ;; esac
  fi
  return 1
}

# writable_dir_by DIR UID -> true if that uid can create files in DIR (w and x).
writable_dir_by() {
  local d="$1" uid="$2" mode
  getfacl -p "$d" 2>/dev/null | grep -qE "^user:${uid}:.wx" && return 0
  mode="$(stat -c '%a' "$d" 2>/dev/null)" || return 1
  case "${mode: -1}" in 3|7) return 0 ;; esac
  if [ "$(stat -c '%g' "$d" 2>/dev/null)" = "$uid" ]; then
    case "${mode: -2:1}" in 3|7) return 0 ;; esac
  fi
  return 1
}

# The gateway edits a Personal memory document only on a human tap on the approver
# bot, never from a tool call. memory.propose_update stages a proposal in
# memory/.proposals/ and posts a card; the tap handler in internal/approval is the
# one caller of the code that appends to a document (see docs/decisions.md,
# 2026-10-30). memory/ is a bind mount of the host's directory, owned by the
# operator, so the gateway (uid 10001) needs an ACL grant on all of it — except
# memory/.git, which the gateway has no business writing. The default ACLs on
# directories keep files the gateway creates writable by the operator, and files
# the operator creates writable by the gateway. .proposals is gitignored and
# created here because nothing else creates it.
prepare_memory() {
  local guid me; guid="$(gateway_uid)"; me="$(id -u)"
  [ -n "$guid" ] || { ylw "could not read the gateway uid — not granting memory/"; return 0; }
  mkdir -p "$ROOT/memory/.proposals"
  find "$ROOT/memory" -path "$ROOT/memory/.git" -prune -o -print0 | xargs -0 setfacl -m "u:${guid}:rwX"
  find "$ROOT/memory" -path "$ROOT/memory/.git" -prune -o -type d -print0 | xargs -0 setfacl -d -m "u:${guid}:rwX,u:${me}:rwX"
  writable_dir_by "$ROOT/memory/taste" "$guid" || die "setfacl on memory/taste did not take — an approved memory tap will fail"
  writable_dir_by "$ROOT/memory/.proposals" "$guid" || die "setfacl on memory/.proposals did not take — propose_update will fail"
}

preflight() {
  local fail=0

  # --- the env files exist -------------------------------------------------
  for f in .env.gateway .env.hermes; do
    [ -f "$DEPLOY/$f" ] || { red "missing $DEPLOY/$f — copy $f.example and fill it in"; fail=1; }
  done
  [ "$fail" -eq 0 ] || return 1

  # --- the two bots' secrets have not been crossed -------------------------
  # chhayad refuses at runtime if it can see TELEGRAM_BOT_TOKEN, but nothing
  # checks the other direction, and nothing checks either before `up`.
  if mentions "$DEPLOY/.env.hermes" CHHAYA_APPROVER_BOT_TOKEN; then
    red "deploy/.env.hermes mentions CHHAYA_APPROVER_BOT_TOKEN."
    red "If the approver token reaches Hermes, the agent can forge its own approvals"
    red "and press its own kill switch. Remove it."
    fail=1
  fi
  if mentions "$DEPLOY/.env.gateway" TELEGRAM_BOT_TOKEN; then
    red "deploy/.env.gateway mentions TELEGRAM_BOT_TOKEN — that belongs to Hermes only."
    red "chhayad would refuse to start anyway; fixing it here saves the restart loop."
    fail=1
  fi

  # --- required values are present (never printed) -------------------------
  has_key "$DEPLOY/.env.hermes"  TELEGRAM_BOT_TOKEN          || { red "deploy/.env.hermes: TELEGRAM_BOT_TOKEN is empty"; fail=1; }
  has_key "$DEPLOY/.env.hermes"  TELEGRAM_ALLOWED_USERS      || { red "deploy/.env.hermes: TELEGRAM_ALLOWED_USERS is empty — with no allowlist Hermes denies everyone"; fail=1; }
  has_key "$DEPLOY/.env.hermes"  PROXY_API_KEY               || { red "deploy/.env.hermes: PROXY_API_KEY is empty — Hermes cannot reach a model without it"; fail=1; }
  has_key "$DEPLOY/.env.gateway" CHHAYA_APPROVER_BOT_TOKEN   || { red "deploy/.env.gateway: CHHAYA_APPROVER_BOT_TOKEN is empty — no approval channel, no kill switch"; fail=1; }
  has_key "$DEPLOY/.env.gateway" CHHAYA_APPROVER_ALLOWED_USERS || { red "deploy/.env.gateway: CHHAYA_APPROVER_ALLOWED_USERS is empty"; fail=1; }

  # --- the model is chosen -------------------------------------------------
  # Not secret, so these live in .env.hermes next to the key they pair with.
  has_key "$DEPLOY/.env.hermes" CHHAYA_HERMES_MODEL    || { red "deploy/.env.hermes: CHHAYA_HERMES_MODEL is unset — e.g. CHHAYA_HERMES_MODEL=claude-sonnet-5"; fail=1; }
  has_key "$DEPLOY/.env.hermes" CHHAYA_HERMES_API_BASE || { red "deploy/.env.hermes: CHHAYA_HERMES_API_BASE is unset — e.g. https://api.anthropic.com/v1"; fail=1; }

  # --- certificates --------------------------------------------------------
  local missing=()
  for c in ca.crt gateway.crt gateway.key hermes.crt hermes.key; do
    [ -f "$DEPLOY/certs/$c" ] || missing+=("$c")
  done
  if [ ${#missing[@]} -gt 0 ]; then
    red "missing certificates: ${missing[*]}"
    red "run: make dev-certs"
    fail=1
  fi

  # --- the gateway can actually read its own key ---------------------------
  # gen-certs.sh grants this at generation time, but it refuses to re-run over
  # existing certs — so every install made before that grant existed lands here.
  # Without this check the symptom is `restart: unless-stopped` crash-looping on
  # "load server keypair: permission denied", which reads like a TLS problem and
  # is not one.
  local guid; guid="$(gateway_uid)"
  if [ -z "$guid" ]; then
    ylw "could not read the gateway uid from deploy/Dockerfile.gateway — skipping the key-readability check"
  elif [ -f "$DEPLOY/certs/gateway.key" ]; then
    if ! traversable_by "$DEPLOY/certs" "$guid"; then
      red "deploy/certs is not traversable by uid $guid, which is what the gateway runs as."
      red "Fix it with:"
      red "  setfacl -m u:${guid}:x $DEPLOY/certs"
      fail=1
    fi
    if ! readable_by "$DEPLOY/certs/gateway.key" "$guid"; then
      red "deploy/certs/gateway.key is not readable by uid $guid, which is what the gateway runs as."
      red "Bind mounts do not remap ownership, so the container sees this file exactly as the host does."
      red "Fix it with:"
      red "  setfacl -m u:${guid}:r $DEPLOY/certs/gateway.key"
      fail=1
    fi
  fi

  # --- Hermes can read its own client key ----------------------------------
  # Same bug, different uid, and a far worse symptom. s6 runs the agent as uid
  # 10000 (`s6-setuidgid hermes`), not as the image's root User. A client key it
  # cannot read does not say "permission denied" anywhere obvious — the MCP
  # handshake just never completes, Hermes blows its 60 s startup deadline, s6
  # SIGTERMs it, and the container crash-loops looking like a network fault.
  if [ -f "$DEPLOY/certs/hermes.key" ]; then
    if ! traversable_by "$DEPLOY/certs" "$HERMES_UID"; then
      red "deploy/certs is not traversable by uid $HERMES_UID, which is what Hermes runs as."
      red "  setfacl -m u:${HERMES_UID}:x $DEPLOY/certs"
      fail=1
    fi
    if ! readable_by "$DEPLOY/certs/hermes.key" "$HERMES_UID"; then
      red "deploy/certs/hermes.key is not readable by uid $HERMES_UID, which is what Hermes runs as."
      red "Hermes will not report this: it fails its MCP handshake, misses its startup"
      red "deadline and crash-loops. Fix it with:"
      red "  setfacl -m u:${HERMES_UID}:r $DEPLOY/certs/hermes.key"
      fail=1
    fi
  fi

  # --- the gateway can stage proposals and apply approved ones -------------
  # `make dev-up` creates and grants these; here we only check. A missing
  # .proposals is fine before the first `up`; present but unwritable means
  # propose_update fails, and an unwritable memory/taste means a tap on approve
  # fails. The fix is the same grant prepare_memory makes.
  local d
  for d in "$ROOT/memory/taste" "$ROOT/memory/.proposals"; do
    if [ -n "$guid" ] && [ -d "$d" ] && ! writable_dir_by "$d" "$guid"; then
      red "${d#"$ROOT"/} is not writable by uid $guid, so memory proposals or approvals will fail."
      red "  find $ROOT/memory -path $ROOT/memory/.git -prune -o -print0 | xargs -0 setfacl -m u:${guid}:rwX"
      fail=1
    fi
  done

  [ "$fail" -eq 0 ] || return 1
  grn "preflight ok"
}

# Render hermes/config.yaml with the model values substituted. We render rather
# than relying on Hermes to expand ${VAR} in its own config, because whether it
# does so for these particular keys is not something we have verified.
render_config() {
  set -a; . "$DEPLOY/.env.hermes"; set +a
  if command -v envsubst >/dev/null 2>&1; then
    envsubst '${CHHAYA_HERMES_MODEL} ${CHHAYA_HERMES_API_BASE}' \
      < "$ROOT/hermes/config.yaml" > "$RENDERED"
  else
    sed -e "s|\${CHHAYA_HERMES_MODEL}|${CHHAYA_HERMES_MODEL}|g" \
        -e "s|\${CHHAYA_HERMES_API_BASE}|${CHHAYA_HERMES_API_BASE}|g" \
        "$ROOT/hermes/config.yaml" > "$RENDERED"
  fi
  # 0644, not 0600. The Hermes container does not run this file's owner: s6 starts
  # the agent with `s6-setuidgid hermes`, uid 10000, and a bind mount passes host
  # ownership through unremapped — so a 0600 file renders fine and then fails at boot
  # with "Your settings file could not be read ... Permission denied", after which
  # Hermes silently runs on defaults. That is the dangerous part: sampling,
  # elicitation, the toolsets allowlist and skills.write_approval all revert.
  #
  # Readable rather than ACL'd for uid 10000 because this file carries no secret —
  # `key_env: PROXY_API_KEY` is a variable name, and the value lives in .env.hermes,
  # which stays 0600. An ACL would also hard-code a uid owned by a pinned third-party
  # image, and break quietly the next time that pin moves.
  chmod 644 "$RENDERED"
  grep -q '\${' "$RENDERED" && die "unsubstituted placeholders remain in $RENDERED"
  grn "rendered hermes config -> ${RENDERED#"$ROOT"/}"
}

# Build the CA bundle Hermes verifies TLS against: the pinned image's own certifi
# bundle, plus our CA.
#
# Hermes hands `ssl_verify` straight to httpx as `verify=`, and httpx 0.28 refuses
# `verify=<path>` together with a client certificate ("Build an ssl.SSLContext and
# pass it as verify=<ctx>") — which a YAML config cannot express. The way in is
# SSL_CERT_FILE, which httpx's default context does honour.
#
# It must be a BUNDLE, never our CA alone: SSL_CERT_FILE REPLACES the trust store,
# so pointing it at ca.crt makes Hermes trust chhaya-ca and nothing else, and the
# Telegram connection dies with CERTIFICATE_VERIFY_FAILED. Measured: certifi alone
# 119 certs, bundle 120, and with the bundle Telegram and the Anthropic API both
# verify while gateway:8443 does too.
#
# Taken from the image rather than the host so the trust set is exactly the one the
# pinned image ships, and rebuilt on every `up` so a pin bump cannot leave a stale
# bundle behind. certifi is asked where its bundle lives; the path contains the
# Python version and must not be hard-coded.
build_ca_bundle() {
  local image bundle
  image="$(sed -n 's/^[[:space:]]*image:[[:space:]]*\(nousresearch\/hermes-agent@sha256:[0-9a-f]*\).*/\1/p' "$DEPLOY/docker-compose.yml" | head -1)"
  [ -n "$image" ] || die "could not read the Hermes image ref from docker-compose.yml"
  bundle="$DEPLOY/certs/ca-bundle.pem"

  docker run --rm --entrypoint sh "$image" -c \
    'cat "$(/opt/hermes/.venv/bin/python -c "import certifi;print(certifi.where())")"' > "$bundle.tmp" \
    || die "could not read certifi's bundle out of $image"
  [ -s "$bundle.tmp" ] || die "certifi bundle came back empty from $image"

  cat "$DEPLOY/certs/ca.crt" >> "$bundle.tmp"
  mv "$bundle.tmp" "$bundle"
  # No secret here — public certificates only — and Hermes reads it as uid 10000.
  chmod 644 "$bundle"

  local n; n="$(grep -c 'BEGIN CERTIFICATE' "$bundle")"
  [ "$n" -gt 1 ] || die "ca-bundle.pem has only $n certificate(s); expected the image's bundle plus ours"
  grn "built certs/ca-bundle.pem ($n certificates: the image's certifi bundle plus chhaya-ca)"
}

cmd_certs() {
  if [ -f "$DEPLOY/certs/ca.key" ]; then
    ylw "certificates already exist in deploy/certs — delete them first if you really mean to rotate"
    return 0
  fi
  "$ROOT/scripts/gen-certs.sh" "$DEPLOY/certs" 2>/dev/null || "$DEPLOY/gen-certs.sh" "$DEPLOY/certs"
}

cmd_up() {
  prepare_memory
  preflight || die "preflight failed — nothing was started"
  render_config
  build_ca_bundle
  "${COMPOSE[@]}" up -d --build
  echo
  grn "stack up. Next:"
  cat <<'NEXT'
  make dev-status      what is running, and whether the gateway is listening
  make dev-logs        follow both containers
  make dev-audit       tail the audit log as decisions happen

  Then, in Telegram, run the smoke test in docs/runbook.md.
NEXT
}

cmd_down()    { "${COMPOSE[@]}" down; }
cmd_restart() { cmd_down; cmd_up; }
cmd_logs()    { "${COMPOSE[@]}" logs -f --tail=80 "$@"; }

cmd_status() {
  "${COMPOSE[@]}" ps
  echo
  # The gateway requires a client certificate, so a plain probe SHOULD be refused.
  # That refusal is the signal we want: it proves mTLS is enforced, not broken.
  if curl -sk --max-time 3 https://127.0.0.1:8443/mcp >/dev/null 2>&1; then
    ylw "gateway answered a request with NO client certificate — mTLS is not being enforced"
  else
    grn "gateway refuses an uncertificated client (expected)"
  fi
  if curl -sk --max-time 3 --cert "$DEPLOY/certs/hermes.crt" --key "$DEPLOY/certs/hermes.key" \
       https://127.0.0.1:8443/mcp >/dev/null 2>&1; then
    grn "gateway accepts Hermes' certificate"
  else
    ylw "gateway did not accept Hermes' certificate — check 'make dev-logs gateway'"
  fi
}

# The audit log lives in a named volume, so read it through a container.
cmd_audit() {
  "${COMPOSE[@]}" exec -T gateway sh -c 'tail -n 40 -f /data/audit.jsonl' 2>/dev/null \
    || ylw "no audit log yet — it appears on the first tool call"
}

# The kill switch, from the path that does not need the daemon to be healthy.
cmd_stop()   { "${COMPOSE[@]}" exec -T gateway chhayactl -data /data stop; }
cmd_resume() { "${COMPOSE[@]}" exec -T gateway chhayactl -data /data resume; }
cmd_verify() { "${COMPOSE[@]}" exec -T gateway chhayactl -data /data audit verify; }

cmd_doctor() { preflight && grn "ready to start"; }

case "${1:-}" in
  up)      cmd_up ;;
  down)    cmd_down ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  logs)    shift; cmd_logs "$@" ;;
  audit)   cmd_audit ;;
  verify)  cmd_verify ;;
  stop)    cmd_stop ;;
  resume)  cmd_resume ;;
  certs)   cmd_certs ;;
  doctor)  cmd_doctor ;;
  *) cat >&2 <<'USAGE'
usage: scripts/dev.sh <command>

  doctor    check env files, tokens, certs — changes nothing
  certs     generate the CA and both certificates (once)
  up        preflight, render Hermes' config, build and start both containers
  down      stop both containers
  restart   down then up
  status    what is running, plus an mTLS probe from both sides
  logs      follow both containers (or one: logs gateway)
  audit     tail the audit log as decisions happen
  verify    walk the audit hash chain from genesis
  stop      kill switch on, via chhayactl (not via the bot)
  resume    kill switch off
USAGE
     exit 2 ;;
esac
