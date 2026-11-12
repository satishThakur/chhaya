# Observability and evals

Status: design, 2026-11-09. Step 1 of the build order (trace export and the annotation page) is built and
tested as of 2026-11-10; everything after it is design only. This document
replaces the label schema and the Langfuse choice in `docs/design.md` (Traces and labeling,
Evals); the learning metrics and the monthly ablation there still stand and come later.

The question this answers: **I changed something. How do I know it got better?**

## The approach in one paragraph

We follow Hamel Husain's error-analysis method. Look at real traces first, write down what went
wrong in my own words, group those notes into failure modes and count them. Fix what is
obviously a gap. For a failure mode that persists or matters, write an eval: a replayable case
built from a real failure, checked by code wherever code can see the failure. An LLM judge is
used only where code cannot see it, and only after it agrees with my own labels. A change ships
when its cases pass more often than the baseline on the same cases, and nothing that passed
before now fails.

## Principles

1. **Data before categories.** Failure modes come from reading traces, not from a list written in
   advance. The nine-item list in `docs/design.md` was written before we had a single trace; it
   is a guess and the counts replace it.
2. **One judge of quality: me.** I am the only user, so my labels are the ground truth. Claude can
   help group notes; it does not write labels.
3. **Pass or fail, not 1 to 5.** Every label and every check is binary, with a note saying why.
   A score of 3 cannot be acted on.
4. **Code checks first.** Most of our failures are visible in the tool calls: did it call memory,
   which path, how many questions. Those are assertions, not opinions.
5. **A judge earns trust.** An LLM judge counts only once it agrees with my labels on traces it
   was not tuned on (see [Judges](#llm-judges-later-and-only-calibrated)).
6. **A baseline before a fix.** Measure the current build on the cases before changing anything.
   Everything is in git, so an older build can be measured too.
7. **Real data stays private.** Traces and cases built from them contain my life. They never enter
   this repo. They live in the private bucket under `evals/`. The repo holds the tooling and
   synthetic cases built from `memory.example/`.

## The flow, and where everything lives

S3 is the system of record for traces and annotations. The desktop only caches.

```
 box, nightly (cron 02:00 IST)
   1. chhayactl backup            -> s3://BUCKET/backups/...        (exists)
   2. chhayactl trace export      -> s3://BUCKET/evals/days/YYYY-MM-DD/trace-<id>.json
                                     reads state.db + audit.jsonl, joins each session with
                                     its gateway calls, writes one trace object per session

 desktop
   3. chhayactl trace serve       local page on 127.0.0.1; opens on the last day's traces,
                                  fetches each trace from S3 as I move next/previous
   4. annotate                    saved to ~/.chhaya/evals/ at once (autosave)
   5. "Export" button             pushes open-code-<id>.json beside its trace in S3
                                  (the page shows how many are unsynced)
```

**Layout in the bucket** (the existing private, encrypted, versioned backup bucket):

| Key | Holds | Written by |
| --- | --- | --- |
| `evals/days/YYYY-MM-DD/trace-<id>.json` | One session: messages, tool calls with results, matched gateway decision per call, unmatched gateway calls flagged, counters | `trace export` (box) |
| `evals/days/YYYY-MM-DD/open-code-<id>.json` | My verdict on that trace: pass or fail, the note, `wanted`, `mode`, and the trace's message count when I annotated | the page's Export button |
| `evals/modes.yaml` | The failure modes (axial coding) | the page |
| `evals/cases/`, `evals/results/` | Eval cases and run results (later) | `eval run` |

**Rules**

- **A trace is one conversation, not one Hermes session.** Hermes keeps a Telegram chat as one session until
  `/new` (core no longer resets on a timer), so a session can hold days of unrelated conversations. The export
  splits a session wherever the silence between two consecutive messages is longer than `-gap` (default 60
  minutes; strictly longer; 0 turns it off). A conversation stays multi-turn: turns are never separated. The first
  conversation keeps the session id (`<id>`), later ones are `<id>.2`, `<id>.3`, each filed under its own IST
  start day. A session that never pauses exports exactly as before. Session totals (tokens, cost) are not
  divisible and stay on the first conversation. Fields: `session_id`, `segment`, `segments`,
  `gap_before_minutes`. Cut by time, so a long-running tool that outlasts the gap would split a call from its
  result (`integrity.orphan_call` would say so); audit entries and approvals go to the conversation they fall
  in (an approval tap in the silence goes to the one before it).
- **What each conversation was sent.** A later conversation's first request is stored in full and marked with
  `carried_messages`: Hermes sent the model the whole session so far, unrelated history included, and the page
  says so. `state.db` keeps only a hash of the system prompt; the text, the tool schemas and what each model
  call was sent come from Hermes' request dumps (see "What the model was sent").
- **Day folder** is the conversation's start date in IST, so "yesterday" is my yesterday.
- **A conversation can grow.** It is re-written whenever the session has new messages (a trace can always be
  regenerated from `state.db`, so overwriting is safe). The annotation records the message count it saw; the
  page shows "new messages since annotated" instead of silently trusting an old verdict.
- **Annotations are overwritten** per trace (latest wins; one user). The bucket's rule that expires
  old versions after 30 days may cover `evals/`; `evals/` needs its own lifecycle rule with no
  expiry (see open questions).
- **Access.** The box's instance role has `PutObject`, `GetObject`, `DeleteObject` on `bucket/*`
  and `ListBucket` (policy checked 2026-11-10), which covers `evals/`. Containers cannot reach the
  role: IMDS hop limit is 1. The desktop uses the operator's own credentials. Upload and download
  shell out to the `aws` CLI, as backup does.
- **A failed export never fails the backup, and the reverse.** Each logs its own result.

Replay is the fast signal and decides whether to ship. The weekly rate in production is the true
signal and says whether we were right. At about 150 tasks a month the weekly rate is slow and
noisy, which is why replay exists.

## Part 1: Observability (the trace)

### What a trace is

One Hermes session, which is one task if I use `/new` at task boundaries (`docs/design.md`,
"`/new` is load-bearing"). A trace holds: the trigger (my message or a cron run), every message,
every tool call with its arguments and result, the gateway's policy decision and approval for
each gateway tool call, and tokens and cost for the session.

### Where the data is today

Verified on the desktop's dev data, 2026-11-09:

| Source | What it holds | Notes |
| --- | --- | --- |
| Hermes `state.db` (`$CHHAYA_HERMES_HOME/state.db`) | `sessions`: id, `source` (`telegram`, `cron`), start/end, model, `system_prompt`, message and tool-call counts, tokens, cost, title. `messages`: per session, `role`, `content`, `tool_calls` (JSON), `tool_name`, `timestamp`, `reasoning`. | The trace's backbone. Tool calls to the gateway appear as `mcp__chhaya__memory_get` and so on. Schema belongs to Hermes and can change on an upgrade. |
| Gateway `audit.jsonl` (`$CHHAYA_GATEWAY_HOME/audit.jsonl`) | Per gateway tool call: `seq`, `ts`, `actor`, `tool`, `kind`, `tier`, `args` (redacted), `decision`, `rule`, `reason`, `taint`, `approval`, `result`, `duration_ms`, hash chain. | Hash-chained and complete by construction. **Carries no session id.** |
| Personal memory (`$CHHAYA_GATEWAY_HOME/memory`, a git repo) | The state memory calls read from; each approved write is a commit. | Lets a case pin memory to a commit. |

Not built, and not needed for the first steps: the model proxy that would log every LLM call, the
session id on gateway spans, Langfuse.

### Joining state.db and the audit log

The audit log has no session id, so the join is by time: a gateway call belongs to the session
whose tool-call message for the same tool sits nearest before it, within a few seconds. With one
user and one chat this is unambiguous in practice. `trace show` prints the join and flags any
gateway call it could not match, rather than guessing. If unmatched calls show up, the fix is to
carry the session id to the gateway (a header Hermes already supports, `session_affinity_header`,
is a model-provider setting, so this needs checking).

### `chhayactl trace`

Reads with the `sqlite3` CLI (`-readonly -json`), so no SQLite driver enters our Go code, and never
writes to the data it reads.

```
chhayactl trace export [-home DIR] [-s3 URI] [-since 2d]   build trace objects from state.db and
                                                           audit.jsonl; upload to evals/days/...
chhayactl trace serve  [-s3 URI] [-dir ~/.chhaya/evals] [-port 8787]
                                                           the annotation page on 127.0.0.1
```

`trace list | show | stats` (the same traces as terminal text) were in the first draft of this design
and are not built: the page covers reading, and `trace export`'s JSON is the input for scripts.

`export` also runs on the desktop against a restored backup (`-home` points at it, `-s3` is
omitted to write files only), which is how it is tested and how an old backup is analysed. The
box runs it nightly at 02:45 IST from `deploy/crontab.box`, after the backup.

### The page (`trace serve`)

A local web page, plain HTML and JS embedded in the `chhayactl` binary, bound to `127.0.0.1` only.
It never faces the internet and has no login because nothing outside this machine can reach it.

- **One conversation at a time**, newest day first, with Previous and Next across traces and days.
  A header shows the day, the position ("4 of 11"), source (telegram or cron), cost and whether it
  is annotated.
- **The conversation as text**: user and assistant turns; each tool call collapsed with arguments,
  result and the gateway's decision; unmatched gateway calls shown in red.
- **Annotation box**: Pass or Fail, the first thing that went wrong, what I wanted, and a mode
  picker once modes exist. Every change saves to disk at once.
- **Export button** pushes every unsynced `open-code-*.json` to S3 and shows "N unsynced" until it
  does. If the desktop is lost before pushing, the count is the only warning, so it stays visible.
- **Data interface**, three calls: list days and traces, get a trace, put an annotation. The local
  server implements them over the `aws` CLI and the cache. A later phone version using presigned
  URLs would implement the same three calls without a page rewrite.

## Part 2: Error analysis

### Open coding: notes

For each trace, one record:

| Field | Values |
| --- | --- |
| `session` | session id |
| `pass` | `true` or `false`: would I be happy if every task went like this? |
| `note` | Free text. If it failed, the **first** thing that went wrong, in my words. Later failures are usually consequences of the first. |
| `wanted` | Optional: what I wanted instead. |
| `mode` | Empty at first. Filled in during axial coding. |

Saved by the page as `open-code-<id>.json` (autosaved locally, pushed with Export), next to the
trace it describes. One object per trace; the latest save wins.

Read every trace while volume allows (it does: about 5 a day). Stop a pass when new traces stop
producing new kinds of notes.

### Axial coding: failure modes

Group the failing notes into a small set of named modes, each with a one-line definition and an
example session. Stored as `evals/modes.yaml` in the bucket. The page assigns a mode to a trace;
`chhayactl trace modes` prints the count per mode and the sessions in each.

Example of what we expect from evidence we already have (`docs/learnings.md`, entry 1):

```yaml
- id: didnt-look
  definition: asked me for something without first checking Personal memory
  example: 94541f
- id: didnt-find
  definition: checked memory but did not reach the entry that held the answer
  example: 94541f
```

Re-run open coding after any big change (a model upgrade, a new tool, a SOUL.md rewrite); new
modes appear after changes.

### What each mode gets

| Situation | Action |
| --- | --- |
| A plain gap: a missing tool, an instruction the prompt never gave | Fix it. Still write the cases first so the fix has a baseline. |
| Recurring, or one I would mind seeing again | Cases plus a code check. |
| Only visible in the wording or taste of the reply | Cases plus an LLM judge, after calibration. |
| Rare and harmless | Leave it; it stays in the counts. |

## Part 3: Evals

### Tiers, mapped to what exists

| Tier (`docs/design.md`) | State |
| --- | --- |
| L0 Ops: the plumbing worked | Partly: `make dev-doctor`, `backup status`. A daily check comes later. |
| L1 Safety: policy holds | Built: `make evals`, assertions on gateway decisions. Unchanged by this doc. |
| L2 Failure modes: past mistakes stay fixed | **This document.** Cases from error analysis, replayed, checked by code. |
| L3 Taste | Later. Needs a calibrated judge. |

### A case

```yaml
id: coffee-near-home-01
mode: didnt-look
source: 94541f                      # session it came from; empty for synthetic cases
memory: 3f2c9ab                     # commit in the Personal-memory repo to replay against
input: "Find me a good coffee place near home"
checks:
  - memory_called_before_question: true
  - read_path: location            # memory.get on this path, or a memory.list that returned it
  - max_questions: 1
runs: 5
```

Single-turn first. The failures we have seen happen on the first turn of a task, and single-turn
cases need no conversation history. Multi-turn cases (prior turns, then the input) come later,
using Hermes' `--resume` on a seeded session.

Real cases live in the bucket under `evals/cases/`. Synthetic cases built only on `memory.example/`
live in this repo under `evals/cases/` so the harness itself is tested in public.

### Checks

Go functions over the parsed trace of the replayed session. Each returns pass or fail and a
one-line reason. First set:

| Check | Passes when |
| --- | --- |
| `memory_called_before_question` | A `memory.*` call happens before the first assistant message that asks me something. |
| `read_path: P` | `memory.get` was called on `P`, or `memory.list` returned `P` and the reply uses it. |
| `called: TOOL` / `not_called: TOOL` | The tool was or was not called. |
| `max_questions: N` | The assistant asked at most N questions. |
| `reply_matches: REGEX` / `reply_not_matches: REGEX` | The final reply does or does not match. |
| `no_write` | No `memory.propose_update` (for cases where nothing should be remembered). |

"Asks me something" means the message ends a turn and contains a question mark. That is crude and
will be wrong sometimes; `trace show` prints which message it counted, so a wrong call is visible.

### Synthetic cases

Real failures are few, and a fix tested only on the cases that prompted it overfits. For each
mode, write synthetic cases by crossing a few dimensions, then keep the ones that look like
something I would actually say. For memory recall:

| Dimension | Values |
| --- | --- |
| Kind of fact | address, a food preference, a person, a standing decision |
| How directly it is asked | names the fact ("my address"), implies it ("near home"), needs two facts |
| Is the fact in memory | yes, no |

The "no" cases matter: the right behaviour there is to look, not find, and then ask once. Without
them, a change that makes the agent call memory on every turn looks like a pure win.

### The replay runner

```
chhayactl eval run     -cases DIR [-runs 5] [-label NAME]    run every case, write results
chhayactl eval compare RESULTS_A RESULTS_B                   per-case pass counts side by side
```

For each case and each run:

1. **Fresh state.** A new temporary Hermes home with our rendered `config.yaml` and `SOUL.md`, and
   no built-in memory, skills or sessions from production, so runs cannot leak into each other.
   A temporary clone of the Personal-memory repo checked out at the case's `memory` commit.
2. **An eval gateway** on its own port and compose project, pointed at that memory clone. Its
   writes must never reach the approver bot (see open questions).
3. **One Hermes turn**, non-interactive: `hermes chat -q INPUT --oneshot --format stream-json
   --source eval` in the pinned image.
4. **Read the trace** from the temporary `state.db` (the same reader as `chhayactl trace`), run the
   checks, record the result, throw the temporary state away.

Each result line records: case id, run number, pass or fail per check with reasons, the session
id, the repo commit, the Hermes image digest, the model, tokens and cost. Results go to
`evals/results/<timestamp>-<label>.jsonl` in the bucket.

**Deciding.** Each case reports k of n runs passed. With 5 runs per case this is coarse, so the
rule is simple and conservative:

- **Improved:** the target mode's cases pass more often, summed across cases.
- **Regression:** any case that passed at least 4 of 5 in the baseline passes 2 or fewer now.
- Ship when improved and there is no regression. `eval compare` prints both.

**Baseline.** Run the current `main` before the change, with `-label baseline`. For a change
already shipped, build the earlier commit and run that (for example, the gateway before
`memory.list`, to measure what it bought).

**Cost.** Every run is real model calls. 20 cases × 5 runs is 100 single-turn conversations per
side of a comparison. `eval run` prints the estimated cost of the previous run before starting.

### LLM judges (later, and only calibrated)

For failure modes code cannot see (tone, taste, whether a recommendation fits me):

1. Label at least 50 traces for that one mode myself, pass or fail.
2. Split them: a few as examples in the judge prompt, the rest held back.
3. The judge answers pass or fail with a reason, for that one mode only, not "quality".
4. Measure on the held-back traces how often it agrees with me on my passes and on my failures,
   separately. Both must be high before it is used; the failures matter more.
5. Re-check agreement after any change to the judge prompt or model.

## Production measurement

Weekly, from my notes: traces labeled, share passed, count per failure mode. Per-session counters
from `trace stats` show drift between labels (for example, memory calls before the first question
falling to zero). With about 30 tasks a week the rates are noisy; report them with a credible
interval (beta-binomial, as `docs/design.md` already says) and treat a change as real only when
the intervals separate.

## Build order

Each step ends with something I can use.

1. **`trace export` and `trace serve`** together (built 2026-11-10; see "Step 1: what was verified"). Done when the box exports last night's sessions
   to `evals/days/...` and the page on the desktop shows them one at a time with Previous and Next,
   gateway decisions matched or flagged.
2. **Open coding and modes.** The page's annotations and Export, `evals/modes.yaml`,
   `trace modes`, and the `evals/` lifecycle rule. Done when I have coded every existing session
   and have counts per mode.
3. **Spike: one non-interactive Hermes turn** against an eval gateway on the desktop. Done when a
   single `hermes chat -q --oneshot` produces a session in a temporary `state.db` with a gateway
   tool call in it. This settles the main unknown before the runner is built.
4. **`chhayactl eval run | compare`** with the first checks, cases for the top mode, and a
   recorded baseline on current `main`.
5. **The fix** for the top mode, compared against the baseline.
6. **Weekly report** from notes and counters.

Later, when the need shows up in the data: multi-turn cases, a judge for a taste mode, the model
proxy for per-call traces, an annotation UI better than the CLI, the monthly ablation.

## Open questions

- **Writes in the eval gateway.** `memory.propose_update` reaches the approver bot. An eval must
  never send a real approval request. Options: the eval gateway runs with a policy file that
  denies writes (the check sees the attempt in the trace either way), or a gateway mode that
  records instead of asking. A mode is code in the security boundary and must refuse to start when
  `CHHAYA_ENV=prod`; a policy file is not. Leaning to the policy file. Decide at step 3.
- **Does `--oneshot` load our MCP server?** The pinned image has the flags
  (`hermes chat --help`, checked 2026-11-09). Not verified: that a one-shot run loads
  `mcp_servers.chhaya`, writes its session to `state.db`, and that `--format stream-json` carries
  tool calls. Step 3 verifies it.
- **Lifecycle for `evals/`.** Checked 2026-11-10: `expire-old-versions` was bucket-wide
  (`filter {}`), so it would have deleted old versions of annotations after 30 days, and S3
  filters cannot exclude a prefix. `infra/terraform/s3.tf` now scopes the expiry rules to
  `backups/`, `handover/`, `terraform/` and `tools/`; `evals/` is in none of them, so nothing
  under it ever expires. Planned (1 change, 0 destroy); **apply before the first Export.**
- **Delete permission.** The box role can delete any object in the bucket. Versioning limits the
  damage, and containers cannot reach the role. Narrowing `DeleteObject` to `evals/*` is the
  owner's IAM call.
- **The question heuristic.** "Contains a question mark" will misfire. Replace it if the notes show
  it is wrong often.
- **Hermes schema drift.** `state.db` belongs to Hermes. The trace reader should fail loudly on a
  missing column, and a Hermes upgrade should re-run `trace show` on a known session.

## Step 1: what was verified

Built 2026-11-10 in `internal/trace` and `cmd/chhayactl/trace.go`; the page is `internal/trace/web/index.html`,
embedded in the binary.

- **The join, on real data.** Exporting the desktop's 13 sessions gave 12 traces (one session had no
  messages). All 30 gateway tool messages matched an audit entry by tool name and time, and no
  gateway call was left unclaimed. Hermes wraps gateway calls in a `tool_call` envelope and its tool
  results in `<untrusted_tool_result>` tags; the reader unwraps the first and keeps the second as is.
- **Counters agree with what we already knew.** The 2026-11-07 session where it asked for an address
  shows 5 questions and 0 memory calls before the first; the exercise-routine session that looked
  first shows no question.
- **The page, rendered in a browser** (headless Firefox against real traces): conversation, collapsed
  tool calls with gateway decisions, the marker on the counted question, the verdict box.
- **Guards, each mutation-checked** (the test fails when the guard is removed): foreign `Host` refused
  (DNS rebinding), writes need the `X-Chhaya` header (cross-site posts), day and id validated before
  they reach a path or the aws CLI, the body cannot override the trace id, listener is 127.0.0.1 only.
- **Not verified:** the S3 path end to end. The tests use a fake `aws`; nothing has uploaded to the real
  bucket yet, and the box has not run the export. The first real run should be on the box with
  `-s3`, then `serve -s3` on the desktop.

## What the model was sent (trace schema 2)

`state.db` holds the conversation, not the requests. With `HERMES_DUMP_REQUESTS=1` Hermes writes
`sessions/request_dump_<session>_<time>.json` before every model call (reason `preflight`): the full
system prompt, the tool schemas and every message. `ReadLLM` (`internal/trace/calls.go`) rebuilds
`trace.llm` from them:

- system prompts and tool sets are stored once, keyed by hash; each call stores only the messages
  added since the previous call (checked as a true prefix; if history was rewritten the call is marked
  `reset` and keeps all of its messages);
- a retry of the identical request is folded into one call (`attempts`);
- each call is linked to the assistant message it produced (`reply_message_id`) by tool-call ids and
  text, each message claimed once. The last call has no later request to confirm it, so it links only to
  the final assistant message. A call that cannot be matched stays unlinked and is listed at the bottom;
- error dumps are counted (`other_dumps`) and not shown; unreadable files are listed in `problems`.

The page shows a "What the model was given" box (system prompt, tools) and, above each assistant
message, a "Sent to LLM" panel with the messages added and an expander for the whole request. Each model call is one card: ① the request, ② the reply (with Hermes' `finish_reason`, `tool_calls` or
`stop`; a reply with no text says the model asked for a tool call), ③ the tool results that reply triggered. The raw API response is not recorded: no per-call token usage and
no reasoning (Hermes stores none for this model).

- **Where the flag is set:** `HERMES_DUMP_REQUESTS=1` in `deploy/.env.hermes` (per machine, git-ignored).
  On for the desktop (dev). **Off on the box** until it is decided deliberately.
- **Privacy:** the system prompt contains Personal memory. Hermes redacts secrets in dumps before writing.
  The dumps sit in plaintext under `~/.chhaya/hermes/sessions/` and, through the trace, in the private
  bucket. Same trust level as the messages already in a trace.
- **Limits:** only sessions run with the flag on have calls. For a session resumed from before the flag
  the first recorded call carries all earlier history. Dump timestamps have no zone (the container's local
  time) and are shown as written. Dumps are pruned with their session by Hermes.

## Code checks (`chhayactl trace check`)

Deterministic checks over exported traces: plain Go over the JSON, no model, no Hermes run, the same
traces always give the same answer. `internal/check`. Run it with `-s3` to check the bucket's copy
(a temporary directory is used so a stale local file is never read instead), `-day` for one IST day,
`-json` for a machine-readable report, `-v` to include `info` lines. It exits non-zero on any `fail`
finding **and** on any trace it could not read: a check that did not run is not a pass.

| Check | Level | Runs on | Catches |
| --- | --- | --- | --- |
| `integrity.unmatched` | fail | all | A gateway call not tied to a tool message (the trace is wrong) |
| `integrity.orphan_call` | fail | all | A tool call with no result |
| `integrity.llm_link` | flag | all | A recorded model call with no reply, or an unreadable request dump |
| `secrets.in_trace` | fail | all | Key- or token-shaped strings anywhere in the trace; reports kind and place, never the value |
| `reply.unanswered_human_message` | fail | telegram | A message you wrote got no text reply before your next one. Injected user-role messages (`[Cron delivery`, `[ASYNC DELEGATION`, `[System note`, `[SYSTEM`, `[IMPORTANT:`) are not you. The last message of a trace exported under 10 minutes ago is given the benefit of the doubt |
| `tool.error` | flag | all | A tool result whose JSON carries a non-empty `error`, `status: error` or `success: false`. Parsed, not text-matched (`"error": null` is fine) |
| `memory.miss` | flag | telegram | `memory.get` on a document that does not exist: a guessed path |
| `loop.repeat_call` | fail | all | The same tool with the same arguments three or more times (key order ignored) |
| `session.not_reset` | flag | telegram | A conversation that began with a message you wrote after more than an hour of silence in the same session: a reset policy would have ended it. The check for whether the idle reset works |
| `tools.surface` | info | all | Non-gateway tools used. Inventory only until there is an allow-list |
| `ask.before_memory` | flag | telegram | A question asked with no memory call first. **Heuristic**: the question detector is crude and the question may not have needed memory |

Levels: `fail` is an invariant, `flag` means look, `info` is inventory. The level belongs to the check
and a check cannot raise its own findings.

**Not decided yet:** the allow-list that would turn `tools.surface` from inventory into a verdict
(which Hermes built-ins are acceptable in chat versus cron); thresholds for tool-call counts or
latency (reported as numbers only until there is a baseline); where results are stored
(`evals/results/` in S3, later).

**Calibration comes next.** A check earns its place by agreeing with your annotations: one that never
fires on traces you marked fail is a blind spot, one that fires on traces you marked pass is noise and
is fixed or removed. That table needs annotated traces; none exist yet.

**First run (2026-11-12, 19 traces, 13 Telegram and 6 cron):** 0 integrity failures, 0 secrets,
3 failures (2 unanswered messages, both real; one `cronjob_manage` called four times with the same
arguments), 8 traces with tool errors, 7 memory misses across 2 traces, `ask.before_memory` on 6 of 13
Telegram traces. These are findings to read, not a score.

## Known limits of step 1

- **The question heuristic is crude**, as predicted: a closing offer ("Want me to walk through
  the setup?") counts as a question. The page marks which message was counted so a wrong call is
  visible; it is not corrected in code.
- **Cost shows $0.000** for sessions where Hermes did not record one; treat it as unknown.
- **Cron sessions** appear as traces too (source `cron`), with long ids; they are in the list.
- **`evals/` lifecycle:** fixed in Terraform, applied by the owner before the first Export (see open questions).
- **The mode field is free text** until `modes.yaml` exists (step 2).
