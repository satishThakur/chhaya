#!/usr/bin/env bash
# Back up and restore Chhaya's data (~/.chhaya/{hermes,gateway}) as one verified tarball.
#
#   scripts/chhaya-backup.sh create            stack must be down; writes tarball + .sha256 + .manifest
#   scripts/chhaya-backup.sh push FILE S3URI   upload FILE, its .sha256 and .manifest
#   scripts/chhaya-backup.sh pull S3URI DIR    download them into DIR
#   scripts/chhaya-backup.sh restore FILE      verify checksum, extract into ~/.chhaya, compare counts
#
# The manifest records what was in the data when it was taken (db integrity, message and
# session counts, audit record count, memory git head). `restore` recomputes the same
# numbers from what it extracted and refuses to call it good if any differ.
# Restore never overwrites: it stops if ~/.chhaya/hermes or ~/.chhaya/gateway already has
# anything in it. docs/deployment.md, step 5.
set -euo pipefail

HOME_DIR="${CHHAYA_HOME:-$HOME/.chhaya}"
BACKUPS="$HOME_DIR/backups"
die() { printf 'error: %s\n' "$1" >&2; exit 1; }

# Prints key=value lines describing the data under $1. Read-only.
manifest_of() {
  local root="$1"
  python3 - "$root" <<'PY'
import sqlite3, sys, os
root = sys.argv[1]
def ro(p): return sqlite3.connect("file:%s?mode=ro" % p, uri=True)
for db in ("state.db", "kanban.db"):
    p = os.path.join(root, "hermes", db)
    if os.path.exists(p):
        c = ro(p)
        print("%s.integrity=%s" % (db, c.execute("pragma integrity_check").fetchone()[0]))
        if db == "state.db":
            for t in ("messages", "sessions", "gateway_routing", "delivery_obligations"):
                print("state.%s=%d" % (t, c.execute("select count(*) from %s" % t).fetchone()[0]))
a = os.path.join(root, "gateway", "audit.jsonl")
print("audit.lines=%d" % (sum(1 for _ in open(a)) if os.path.exists(a) else 0))
PY
  printf 'memory.head=%s\n' "$(git -C "$root/gateway/memory" rev-parse HEAD 2>/dev/null || echo none)"
  printf 'memory.dirty=%s\n' "$(git -C "$root/gateway/memory" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
  printf 'cron.jobs=%s\n' "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(len(d.get("jobs",d)))' "$root/hermes/cron/jobs.json" 2>/dev/null || echo 0)"
}

stack_is_down() {
  command -v docker >/dev/null || return 0
  ! docker ps --format '{{.Names}}' 2>/dev/null | grep -Eq '(^|[-_])(hermes|gateway)([-_]|$)'
}

cmd_create() {
  stack_is_down || die "the stack is running. make dev-down first: a live state.db is not a consistent backup"
  [ -d "$HOME_DIR/hermes" ] && [ -d "$HOME_DIR/gateway" ] || die "no data under $HOME_DIR"
  mkdir -p "$BACKUPS"; chmod 700 "$BACKUPS"
  local ts name out
  ts="$(date -u +%Y%m%d-%H%M%S)"; name="chhaya-data-$(hostname -s)-$ts"; out="$BACKUPS/$name"
  manifest_of "$HOME_DIR" > "$out.manifest"
  grep -q 'state.db.integrity=ok' "$out.manifest" || die "state.db failed its integrity check; see $out.manifest"
  if command -v go >/dev/null && [ -f "$(dirname "$0")/../go.mod" ]; then
    (cd "$(dirname "$0")/.." && go run ./cmd/chhayactl -data "$HOME_DIR/gateway" audit verify) | sed 's/^/audit.verify=/' >> "$out.manifest"
    grep -q 'audit.verify=ok' "$out.manifest" || die "audit chain did not verify"
  else
    echo "audit.verify=skipped" >> "$out.manifest"
  fi
  # Lock files carry a pid that means nothing on another machine; sockets are not data.
  tar -C "$HOME_DIR" --exclude='*.lock' --exclude='*.sock' -czf "$out.tar.gz" hermes gateway
  chmod 600 "$out.tar.gz" "$out.manifest"
  (cd "$BACKUPS" && sha256sum "$name.tar.gz" > "$name.tar.gz.sha256")
  tar -tzf "$out.tar.gz" >/dev/null || die "the tarball we just wrote does not read back"
  echo "wrote $out.tar.gz"; cat "$out.manifest"
}

cmd_push() {
  local f="$1" dest="${2%/}"
  [ -f "$f" ] && [ -f "$f.sha256" ] && [ -f "${f%.tar.gz}.manifest" ] || die "need $f, $f.sha256 and the .manifest beside it"
  for x in "$f" "$f.sha256" "${f%.tar.gz}.manifest"; do aws s3 cp --only-show-errors "$x" "$dest/$(basename "$x")"; done
  echo "uploaded to $dest/"
}

cmd_pull() {
  local src="${1%/}" dir="$2"
  mkdir -p "$dir"; chmod 700 "$dir"
  aws s3 cp --only-show-errors --recursive "$src/" "$dir/"
}

cmd_restore() {
  local f="$1"
  [ -f "$f" ] || die "no such file: $f"
  for d in hermes gateway; do
    [ -z "$(ls -A "$HOME_DIR/$d" 2>/dev/null)" ] || die "$HOME_DIR/$d is not empty; restore never overwrites"
  done
  (cd "$(dirname "$f")" && sha256sum -c "$(basename "$f").sha256") || die "checksum mismatch"
  mkdir -p "$HOME_DIR"; chmod 700 "$HOME_DIR"
  tar -C "$HOME_DIR" --no-same-owner -xzf "$f"
  chmod 700 "$HOME_DIR/hermes" "$HOME_DIR/gateway"
  local want="${f%.tar.gz}.manifest" got; got="$(mktemp)"
  manifest_of "$HOME_DIR" > "$got"
  # audit.verify is recorded at backup time and re-run by the operator; everything else must match.
  if diff <(grep -v '^audit.verify' "$want") "$got"; then
    echo "restore verified: every count matches the manifest"
  else
    die "restored data differs from the manifest (diff above)"
  fi
}

case "${1:-}" in
  create)  cmd_create ;;
  push)    [ $# -eq 3 ] || die "usage: push FILE S3URI"; cmd_push "$2" "$3" ;;
  pull)    [ $# -eq 3 ] || die "usage: pull S3URI DIR"; cmd_pull "$2" "$3" ;;
  restore) [ $# -eq 2 ] || die "usage: restore FILE"; cmd_restore "$2" ;;
  *) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
