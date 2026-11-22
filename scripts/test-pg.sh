#!/usr/bin/env bash
# Runs the TimescaleDB-backed tests of internal/health against a throwaway container.
set -euo pipefail
NAME=chhaya-test-pg
PORT=55432
cleanup() { docker stop "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker stop "$NAME" >/dev/null 2>&1 || true
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=test -p "127.0.0.1:$PORT:5432" timescale/timescaledb:2.17.2-pg16 >/dev/null
for _ in $(seq 1 40); do docker exec "$NAME" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 2; done
sleep 3 # the image restarts the server once after init
cd "$(dirname "$0")/.."
CHHAYA_TEST_PG_URL="postgres://postgres:test@127.0.0.1:$PORT/postgres" go test ./internal/health/ -count=1 -v -run PG
