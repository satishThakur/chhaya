# The health module

**Decided:** 2026-11-22 (`decisions.md`). **Status:** built and tested end to end on the dev stack; not yet on the box, and never run against a live Garmin account.

Garmin data goes into TimescaleDB and Hermes reads it through two tools. The module lives **inside the gateway**: the same process, the same audit log, the same kill switch. Two small helpers run beside it: the database, and the sync that fills it.

```
Garmin Connect
   │ garminconnect (Python, in its own container)
   ▼
garmin-sync ──POST /ingest/day, /ingest/run──►  gateway :8444   (plain HTTP, shared secret, compose network only)
 (every 3 h)        X-Ingest-Secret                │ validate (strict) → audit record → Upsert
                                                   ▼
                                             timescaledb  (hypertables + weekly continuous aggregate)
                                                   ▲
Hermes ──MCP over mTLS──► gateway :8443 ── health.get_summary / health.get_series (read-only, tier 0)
```

## Why it is shaped like this

- **Inside the gateway, not a second service.** One auth boundary, one audit chain, one kill switch. The ingest listener is not an MCP tool, so `policy.Decide` does not apply to it; it holds the same line by other means: a secret Hermes does not have, a kill-switch check, and an audit record for every request it accepts or refuses.
- **Plain HTTP and a shared secret on the compose network** (owner's call: no mTLS between sidecar and gateway). The port is not published to the host. Hermes shares the network, so the secret is what keeps it out; `scripts/dev.sh` refuses to start if `.env.hermes` mentions any health secret.
- **TimescaleDB, kept on purpose.** The data is a time series and will grow (minute-level heart rate, stress and body battery are the next streams). A narrow `health_metrics(time, metric, value)` hypertable takes those without a schema change, and the weekly rollup is a continuous aggregate that includes days not yet materialised.
- **The sidecar was rewritten, not copied** (see "What the old pipeline got wrong").

## Data

| Table | Holds | Key |
| --- | --- | --- |
| `health_metrics` (hypertable, 90-day chunks) | one number per named metric per local day, stamped at that day's midnight in `CHHAYA_HEALTH_TZ` (default `Asia/Kolkata`) | `(metric, time)`, upsert |
| `health_activities` (hypertable) | one row per workout | `(type, time)`, upsert |
| `health_sync_runs` | one row per sidecar pass: status, days, counts, errors | `id` |
| `health_metrics_weekly` (continuous aggregate) | mean, min, max, days with data per 7-day bucket per metric | |

26 metrics: sleep (total, deep, light, rem, awake seconds, score, avg hrv), HRV (last night, weekly), stress (avg, max, rest seconds), daily stats (steps, calories total/active/bmr, distance, resting/min/max heart rate, moderate and vigorous intensity minutes) and body battery (charged, drained, high, low). Each has a range in `internal/health/payload.go`; a value outside it is refused, because one bad number poisons every average after it.

**A field the sidecar did not send is absent, never 0.** A day with no data is a gap in the series. Sleep that was not recorded is not "0 hours".

## Ingest

`POST /ingest/day` takes one local day: `{date, sleep?, hrv?, stress?, daily_stats?, body_battery?, activities?}`. Every section and field is optional; the decoder rejects unknown fields and trailing data, the day must hold something, and the date may be at most one day ahead. The day is written in one transaction, so it lands whole or not at all. Re-sending a day replaces it, which is how today grows through the day.

`POST /ingest/run` records how a pass went (`status` ok, partial or failed, counts, up to 20 errors). The gateway keeps it in `health_sync_runs` and writes `~/.chhaya/gateway/health-sync.json`, the record the morning report reads.

Refusals: 401 bad or missing secret, 503 kill switch on, 400 an invalid day (the body names the field), 413 over 1 MiB, 502 the store refused, 500 the audit log is not writable (a write with no record is not reported as a success). Every one of those, and every success, is an audit record with actor `garmin-sync`; records carry counts and the date, never values.

## Tools

`health.get_summary(days=7)` and `health.get_series(metric, days=30, bucket=day|week)`. Tier 0, read, Trusted (the store holds the owner's own readings and only the sidecar can write them). The summary reports what was measured and no verdict: per metric the days with data, mean, min, max and latest; from six days up the mean of the earlier and the later half; workouts counted by type; `stale` and `days_behind`; and the last sync's status. No confidence numbers (the old app invented 0.85 and 0.78). The dreamer and the approver are refused both tools by allowlist, with tests.

## The old pipeline (`~/codebase/health-assistant`): what we kept and what was wrong

Kept: the idea (Python fetch, Go validate and store, TimescaleDB), the Garmin field names, the validators' bounds. Left: JWT, Google sign-in, the Flutter app, the other services.

Defects found by reading it, each now covered by a test:

1. **HRV could never ingest.** The sidecar sent `last_night_avg`, `weekly_avg`, `status`; the validator demanded `average_hrv`. Every post was refused. `internal/health/contract_test.go` runs the real sidecar transform and feeds its output to the gateway's strict decoder; reintroducing the mismatch fails it.
2. **At HEAD, no sync could land at all**: the ingest routes needed `X-Ingest-Secret` (added 2026-02-18) and the sidecar never sent it.
3. Reading `hrvSummary.lastNightAvg` from the top level, body battery `highest`/`lowest` keys Garmin does not send, activity `startTimeGMT` as epoch milliseconds when it is a string, local-time `fromtimestamp` stamped `Z`, and **every fetch error swallowed into "no data"**. The new sidecar keeps raising separate from "nothing there", and reports the section that failed.
4. A password logged by length and first two characters. Gone. Tokens are saved after the first password login and reused, so Garmin is not asked for a password every pass.
5. `daily_metrics` grouped by the JSONB payload, so the rollup did nothing. The new aggregate groups by metric and week.

## Running it

Enable (once per environment):

```
scripts/dev.sh health-init          # writes deploy/.env.health with generated DB password and ingest secret
$EDITOR deploy/.env.health          # put in GARMIN_EMAIL and GARMIN_PASSWORD
scripts/dev.sh restart              # starts timescaledb and garmin-sync with the gateway
```

First live run, before trusting it (nothing is posted):

```
scripts/dev.sh compose run --rm garmin-sync python -m garmin_sync.main --dry-run
```

It signs in, fetches yesterday and today, and prints which sections it would post and how many numbers each holds. Check the counts against the Garmin app. If a section is missing, Garmin's response differs from what the transform expects (Garmin's API is undocumented): fix `garmin_sync/transform.py` and add the real shape as a fixture.

Watching it: the 07:30 IST morning report has a "health sync" row (FAILED if the gateway has no record, the record is over 26 hours old, or the last pass was partial or failed). The audit log shows every ingest (`scripts/dev.sh audit`).

**Backup.** The database files sit on the host at `~/.chhaya/health/pgdata` (a bind mount, owned by the image's postgres uid, so the tarball does not take them), so `scripts/dev.sh backup` first runs `pg_dump` into `~/.chhaya/gateway/health.dump`, which the tarball carries. A failed dump stops the backup before the stack is touched. `scripts/dev.sh health-dump` does it by hand. **Restore** into an empty database: `scripts/dev.sh health-restore [FILE]` (types RESTORE to confirm; follows TimescaleDB's pre/post-restore steps; tested on a real round trip).

Tests: `make test` runs everything that needs no database, including the Python-to-Go contract test (skipped only if there is no `python3`). `make test-pg` starts a throwaway TimescaleDB and runs the store tests, which cannot be faked. The Python unit tests: `cd sync/garmin && python3 -m unittest discover -s tests`.

## Not done

- **Unverified against live Garmin.** Everything above is tested with fixtures shaped from the old code and from `garminconnect`'s documented behaviour. The `--dry-run` is the first real check.
- Evals specific to health (answers quote only returned numbers; a stale store is said to be stale; no medical advice), to be written from real sessions.
- A proactive daily nudge (needs a decision on what is worth a message).
- HRV status ("balanced") and sleep stages over the night; both need a non-numeric or finer-grained store.
- Reading health data back into the trace checks (a check that an answer's numbers appear in the tool output).

## Where the sync runs: the desktop, through S3

Garmin refuses sign-ins from AWS addresses: from the box the password was accepted and the token
step answered HTTP 429 on the very first attempt; the same login from a home address worked first
time (2026-11-22). So the sync runs on the desktop and the box never talks to Garmin.

```
desktop: garmin-sync  --(put-only key)-->  s3://BUCKET/health/inbox/{day-DATE-HASH,run-TS-HASH}.json
box:     cron every 15 min: chhayactl health pull  -->  gateway 127.0.0.1:8444 (strict decoder)
                                                   -->  files under health/processed/ or health/rejected/
```

- **Desktop:** `deploy/.env.health` holds the Garmin login, `HEALTH_S3_URI=s3://BUCKET/health/inbox` and a
  put-only key (`HEALTH_AWS_ACCESS_KEY_ID`/`SECRET`). `scripts/dev.sh health-sync-up` starts just the
  sync (`restart: unless-stopped`, so it survives reboots); `health-sync-down` stops it.
- **Box:** no Garmin login, no sync container. `deploy/crontab.box` has the pull line. The gateway publishes
  its ingest on the box's loopback only (`127.0.0.1:8444`): Hermes cannot reach it, the shared secret
  still guards it.
- **The key** is created by hand, not by Terraform (a key made there sits in the state file):
  `aws iam create-access-key --user-name chhaya-health-uploader`. The user may only `PutObject` under
  `health/inbox/`: it cannot read health data, list the bucket or touch backups and traces.
- **A refused document** (the decoder says why) is moved to `rejected/` and sent to you once on Telegram.
  A gateway that is down, or a wrong secret, leaves everything in `inbox/` and the next pass retries.
  `processed/` expires after 14 days; `inbox/` and `rejected/` never do.
- **Desktop off:** nothing arrives, the sync row in the morning report ages, and `health.get_summary`
  already says the data is stale. Each pass fetches today and the two days before it, so a gap of up to
  two days heals itself; for longer, `--once --days N`.
- **Why not post straight to the box?** That needs the ingest port open to the internet or a tunnel. S3
  is already the exchange for traces and backups, with its own access control.

## Sync: when it runs, idempotence, backfill

The sync is a loop in the `garmin-sync` container: every `SYNC_EVERY_MINUTES` (default 180) it signs in (reusing the saved token), fetches **today and the two days before it** from Garmin, posts each day, then posts a run record. It is not a delta from a stored cursor: the window is fixed and the gateway upserts by (day, metric), so re-posting a day replaces it and any run is safe to repeat. Yesterday is refetched because Garmin finishes sleep and body battery after midnight.

- Run it now (on the desktop): `scripts/dev.sh compose run --rm garmin-sync python -m garmin_sync.main --once`
- Look first, post nothing: add `--dry-run`.
- Backfill history once: `... --once --days 30` (up to 365; older days first). Garmin rate-limits, so do big backfills in chunks.
- There is no HTTP trigger: the ingest port only accepts data, and the sidecar holds the Garmin login, so "start a sync" is a command on the desktop (`health-sync-up`, or the `--once` line above), not an API.
