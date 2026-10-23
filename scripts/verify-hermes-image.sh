#!/usr/bin/env bash
# Re-verify the assumptions chhaya's design makes about Hermes, against a
# specific image digest. Run this on every Hermes bump before changing the pin.
#
#   scripts/verify-hermes-image.sh nousresearch/hermes-agent@sha256:<digest>
#
# Exits non-zero if any assumption no longer holds. Each check names the
# decision it protects, so a failure tells you what to go re-read.
set -uo pipefail

IMG="${1:?usage: verify-hermes-image.sh <image@sha256:digest>}"
PASS=0; FAIL=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; FAIL=$((FAIL+1)); }
note() { printf '        %s\n' "$1"; }

run() { docker run --rm --entrypoint python3 -w /opt/hermes "$IMG" -c "$1" 2>&1; }

# grep inside the image without running Hermes' interpreter on our behalf.
img_grep() { docker run --rm --entrypoint grep -w /opt/hermes "$IMG" "$@" >/dev/null 2>&1; }

echo "verifying $IMG"
echo

# ---------------------------------------------------------------- runtime ---
echo "runtime"
PY=$(docker run --rm --entrypoint python3 "$IMG" -V 2>&1 | awk '{print $2}')
case "$PY" in
  *a*|*b*|*rc*) bad "interpreter is a pre-release: $PY" ;;
  "")           bad "could not determine interpreter version" ;;
  *)            ok  "interpreter is a stable release: $PY" ;;
esac

out=$(run "import charset_normalizer, requests; print('ok')")
[ "$out" = "ok" ] && ok "requests/charset_normalizer import cleanly" \
                  || { bad "import failed (this segfaulted on 3.14.0a6)"; note "$out"; }

# ------------------------------------------------- trace correlation (M0) ---
echo
echo "trace correlation  [decisions.md: session_affinity_header]"
HOME_DIR=$(mktemp -d); chmod 777 "$HOME_DIR"
cat > "$HOME_DIR/config.yaml" <<'YAML'
providers:
  chhaya-proxy:
    api: "http://127.0.0.1:4000/v1"
    key_env: PROXY_API_KEY
    transport: chat_completions
    session_affinity_header: x-chhaya-session-id
YAML
out=$(docker run --rm -v "$HOME_DIR":/data -e HERMES_HOME=/data --entrypoint python3 -w /opt/hermes "$IMG" -c "
from agent.opencode_affinity import merge_session_affinity_headers as m, custom_provider_session_affinity_headers as f
U='http://127.0.0.1:4000/v1'
a = m({}, 'chhaya-proxy', U, 'sess-abc123').get('extra_headers', {}).get('x-chhaya-session-id')
b = f('https://api.openai.com/v1', 'sess-abc123')
print('STAMPED' if a == 'sess-abc123' else 'MISSING', 'CLEAN' if b == {} else 'LEAKED')
" 2>&1 | tail -1)
# Hermes seeds this dir as root inside the container, so clean up from a
# container too rather than leaving root-owned litter in /tmp.
docker run --rm -v "$HOME_DIR":/d --entrypoint sh "$IMG" -c 'rm -rf /d/* /d/.[!.]* 2>/dev/null' >/dev/null 2>&1
rmdir "$HOME_DIR" 2>/dev/null
case "$out" in
  "STAMPED CLEAN") ok "session id stamped on our endpoint, not leaked to others" ;;
  *)               bad "affinity header behaviour changed: $out" ;;
esac

# ------------------------------------------------------- config key shapes ---
echo
echo "config keys  [decisions.md: M0a spike]"

# grep the files in the image rather than importing and inspecting them: we
# are checking that specific shapes are present in Hermes' own source, not
# executing it, so there is no reason to run their interpreter on our behalf.
if img_grep -q 'config.get("sampling", {})' tools/mcp_tool_server_run.py \
   && img_grep -q 'sampling_config.get("enabled", True)' tools/mcp_tool_server_run.py; then
  ok "sampling/elicitation still nested dicts defaulting to on"
else
  bad "sampling/elicitation config shape changed -> re-read before trusting the pin"
fi

# Assert on the implementation, not on comment prose: the wording of the
# config_defaults comment changed between 0.22.2 and 0.22.3 while the
# behaviour did not, and a string match on the comment cried wolf.
if img_grep -q 'required_environment_variables' tools/env_passthrough.py; then
  ok "skill-declared env vars still auto-forwarded (skills.write_approval stays load-bearing)"
else
  bad "skill env passthrough mechanism changed -> revisit skills.write_approval"
fi

# -A30: the "mode" key sits behind a block of explanatory comments in
# config_defaults.py, well past the -A3 window a terser block would need.
out=$(docker run --rm --entrypoint grep -w /opt/hermes "$IMG" -A30 '"approvals"' hermes_cli/config_defaults.py 2>&1 | grep -o '"mode":[[:space:]]*"[a-z]*"' | head -1 | sed 's/.*"\([a-z]*\)"$/\1/')
[ "$out" = "smart" ] && ok "approvals.mode still defaults to 'smart' (we override to manual)" \
                     || note "approvals.mode default is now '$out' -- check our override still makes sense"

# ----------------------------------------------------------- write sandbox ---
echo
echo "write sandbox"
out=$(run "
import os
from agent.file_safety import get_safe_write_roots
print(os.environ.get('HERMES_WRITE_SAFE_ROOT','') + '|' + ','.join(sorted(get_safe_write_roots())))
")
case "$out" in
  "/opt/data|/opt/data") ok "HERMES_WRITE_SAFE_ROOT=/opt/data set by default and resolving" ;;
  *)                     bad "write-safe-root changed: $out" ;;
esac

echo
printf 'result: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || { echo "Do not move the pin until these are understood."; exit 1; }
echo "Safe to pin this digest."
