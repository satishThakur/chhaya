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
  chmod 600 "$RENDERED"
  grep -q '\${' "$RENDERED" && die "unsubstituted placeholders remain in $RENDERED"
  grn "rendered hermes config -> ${RENDERED#"$ROOT"/}"
}

cmd_certs() {
  if [ -f "$DEPLOY/certs/ca.key" ]; then
    ylw "certificates already exist in deploy/certs — delete them first if you really mean to rotate"
    return 0
  fi
  "$ROOT/scripts/gen-certs.sh" "$DEPLOY/certs" 2>/dev/null || "$DEPLOY/gen-certs.sh" "$DEPLOY/certs"
}

cmd_up() {
  preflight || die "preflight failed — nothing was started"
  render_config
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
