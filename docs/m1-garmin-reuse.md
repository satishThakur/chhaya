# M1 reuse inventory: `health-assistant`

`~/codebase/health-assistant` is a working personal health app of ours — a Go backend, a
Flutter client, and a Python Garmin sidecar — built between January and March 2026. It
already runs the architecture M1 needs, so M1 is largely a port rather than a build.

This file is the inventory: what we take, what we leave, and what we must not carry over.
The decision it supports is in `decisions.md` (2026-10-22, Garmin access route).

Paths below are relative to `~/codebase/health-assistant` unless noted. They are a snapshot
of commit `5ad58a5`; re-check before copying.

## The shape it validates

```
Garmin Connect
   │  garminconnect / garth, Python
   ▼
garmin-scheduler (sidecar process)     ← Python lives here and only here
   │  HTTP POST, shared-secret header
   ▼
Go service: validate → transform → store
   │
   ▼
event store
```

This is the structure `design.md` describes for Garmin, and it is the reason the
"Python library" problem is not a problem: the library never enters our address space.
For chhaya the third box is the gateway's `health.*` tools rather than a REST API, and the
store is SQLite rather than Postgres, but the boundary and the direction of flow are the same.

## Take

| What | Source | Size | Why it's the valuable part |
|---|---|---|---|
| Garmin field mappings, six data types | `services/garmin-scheduler/app/garmin_client.py` | ~350 lines | The undocumented part. `dailySleepDTO.sleepTimeSeconds`, `avgStressLevel`, `lastNightAvg`, body-battery `charged`/`drained`, millisecond-epoch timestamps. Rediscovering this means days of poking at live responses. |
| Normalised data model | `backend/internal/models/event.go` | ~60 lines | `GarminSleep`, `GarminActivity`, `GarminDailyStats`, `GarminBodyBattery`. Already Go, already the right shape. |
| Payload validators and their tests | `backend/internal/garmin/validator.go`, `validator_test.go` | 265 + 283 | Pure functions, table-driven tests, no I/O. The same shape the gateway's tier validators want. |
| Payload → event transforms | inside `backend/internal/garmin/handler.go` | ~200 lines of a 517-line file | The handlers themselves are HTTP boilerplate we don't want; `transformSleepToEvent` and its five siblings are the logic. |
| Correlation functions | `backend/internal/checkin/repository.go:294-415` | ~125 lines | `sleepEnergyCorrelation`, `activityMoodCorrelation`, `sleepFocusCorrelation`. Pure Go over an in-memory map, no database. This is the content of `health.get_summary(days)` and of the daily nudge. |
| Sync audit record | `backend/internal/audit/repository.go:12-28` | ~30 lines | `records_fetched/inserted/updated`, `earliest/latest_timestamp`, `status`, duration. Maps field-for-field onto the L0 ops eval in `design.md` — "sync wrote today's rows and the latest timestamp is fresh". |
| Event-store table design | `scripts/db/init.sql` | — | `(time, event_type, source, data JSON)` with upsert on conflict. The design ports to SQLite; the `pgx` code does not. |

Roughly 750 lines of Go and 350 of Python — about a fifth of the repo, and the fifth that
took longest to get right.

## Leave

- **TimescaleDB and Postgres.** A hypertable, a continuous aggregate and a `pgx` pool for a
  single user. chhaya uses SQLite. Keep the table *design*, drop `backend/internal/db/`.
- **`user_id` everywhere.** chhaya is single-user. The column, the `DEFAULT_USER_ID`
  constant and the UUID plumbing all go.
- **`backend/internal/auth/`, `middleware/auth.go`, `dashboard/handler.go`.** JWT, Google
  Sign-In and REST endpoints for a Flutter client. chhaya has one user, no app, and mTLS.
- **`model-service/`, `mobile_app/`, `poc/`.** Out of scope.

## Do not carry over

1. **The pipeline there is currently broken — check before trusting its data.**
   `middleware.WithIngestSecret` was added to all six `/api/v1/garmin/ingest/*` routes on
   2026-02-18, in the Google Sign-In commit. `ingestion_client.py` last changed 2026-02-11
   and sends no `X-Ingest-Secret` header; `grep -rn "header\|secret" services/garmin-scheduler/app/`
   returns nothing. Every sync POST should have been 401ing since February. The audit leg is
   worse: it posts to `/api/v1/audit/sync`, which sits behind `requireAuth`, and the sidecar
   holds no JWT either. **Lesson for us:** the sidecar's authentication to the gateway needs a
   test that exercises the real boundary, not just the handler.

2. **`garmin_client.py:connect()` logs the password's length and first two characters.**
   Straight violation of "never log secrets". Delete on the way in.

3. **Email and password in the environment, 2FA unsupported** — `get_garmin_tokens.py` says
   so in its own error text. Use garth's token-file auth instead: password used once,
   interactively, then only the refreshable token is stored.

4. **The correlation confidences are invented.** `0.85`, `0.78`, `0.82` are hardcoded
   constants on what is a two-group mean comparison with n≥5 and a 5% threshold — no
   significance test. Fine as a heuristic, wrong as a number. It matters more here than there
   because the output is a Telegram nudge and the bar is "one useful nudge a day and no
   noise". Report the actual quantities instead: *"across 11 days, energy averaged 6.8 after
   7h+ sleep and 5.1 below it."*

5. **`daily_metrics` groups by the JSONB `data` column**, so `COUNT(*)` is ~1 per group and
   the rollup does nothing. Moot under SQLite, but a reason not to copy the SQL wholesale.

## Open for M1, not settled here

- **Trust label for the Garmin store.** `design.md` lists it under Trusted reads, written
  when we assumed an official API. Scraped data is still ours and not attacker-controlled, so
  it probably stays trusted — but the M0b spec makes `trust` an explicit field set at
  registration, so this should be a decision rather than an inherited default.
- **How the sidecar authenticates to the gateway.** mTLS like Hermes, or a shared secret over
  localhost. mTLS is consistent and we already have the machinery.
- **Where the SQLite file lives** and whether the gateway or the sidecar owns writes to it.
  One writer is the simpler rule.
