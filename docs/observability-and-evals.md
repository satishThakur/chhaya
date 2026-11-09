# Observability and evals

Status: design, 2026-11-09. Nothing here is built yet except where it says so. This document
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
   this repo. The repo holds the tooling and synthetic cases built from `memory.example/`.

## The loop

```
  production (box)                    analysis (desktop)                      change
  ────────────────                    ──────────────────                      ──────
  Telegram tasks  ──nightly backup──▶ restored copy of prod data
                                        │
                                        ▼
                                      chhayactl trace list / show     (read traces)
                                        │
                                        ▼
                                      notes: pass/fail + first failure (open coding)
                                        │
                                        ▼
                                      failure modes + counts           (axial coding)
                                        │
                     ┌──────────────────┴───────────────────┐
                     ▼                                      ▼
               obvious gap: fix it                 persistent: write cases
                                                            │
                                                            ▼
                                              chhayactl eval run (baseline)
                                                            │
                                                     make the change
                                                            │
                                                            ▼
                                              chhayactl eval run (candidate)
                                              chhayactl eval compare  ──▶ ship or not
                                                            │
                                                            ▼
                                              production: failure-mode rate per week
```

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

Read-only. Same constraints as backup: it shells out to the `sqlite3` CLI (`-readonly -json`),
so no SQLite driver enters our Go code, and it never writes to the data it reads.

```
chhayactl trace list  [-home DIR] [-since 7d]        one line per session: id, start, source,
                                                     title, turns, tool calls, memory calls,
                                                     cost, label if any
chhayactl trace show  [-home DIR] <session-id>       the conversation as readable text: user and
                                                     assistant turns, each tool call with args and
                                                     result, the matched gateway decision, existing
                                                     notes
chhayactl trace stats [-home DIR] [-since 7d]        per-session counters (below), as a table
```

**Where it runs.** On a restored copy of production on the desktop, not on the live box:

```
chhayactl backup pull -s3 s3://.../backups -dir ~/chhaya-analysis/in
chhayactl backup restore -home ~/chhaya-analysis/prod-20261109 <archive>
chhayactl trace list -home ~/chhaya-analysis/prod-20261109
```

Restore never overwrites, so each pull is a dated copy. Reading prod on the box over SSM also
works (it is read-only), but analysis on a copy can never disturb the live agent.

**Counters, per session** (`trace stats`, and later the weekly report):
turns, tool calls, gateway calls by tool, memory calls before the first assistant question,
questions asked by the assistant, tokens, cost, duration, proposals made and their approval
result.

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

Stored as `~/.chhaya/evals/notes.jsonl`, one JSON object per line, appended by
`chhayactl trace note <session-id> -fail "asked my address instead of checking memory"` (or
`-pass`). A later note on the same session supersedes the earlier one; the file is never edited
in place.

Read every trace while volume allows (it does: about 5 a day). Stop a pass when new traces stop
producing new kinds of notes.

### Axial coding: failure modes

Group the failing notes into a small set of named modes, each with a one-line definition and an
example session. Stored as `~/.chhaya/evals/modes.yaml`. `chhayactl trace note -mode` assigns
one; `chhayactl trace modes` prints the count per mode and the sessions in each.

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

Real cases live in `~/.chhaya/evals/cases/`. Synthetic cases built only on `memory.example/`
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
`~/.chhaya/evals/results/<timestamp>-<label>.jsonl`.

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

1. **`chhayactl trace list | show | stats`** over a restored copy of prod. Done when I can read
   every session from the last backup as text, with gateway decisions matched or flagged.
2. **Notes and modes** (`trace note`, `trace modes`, files in `~/.chhaya/evals`). Done when I have
   coded every existing session and have counts per mode.
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
- **Backups of `~/.chhaya/evals`.** Notes and real cases are worth keeping. `chhayactl backup`
  covers only `hermes/` and `gateway/` and refuses anything else on restore. Either extend it to
  `evals/` (with tests) or make `evals/` its own private git repo. Decide at step 2.
- **The question heuristic.** "Contains a question mark" will misfire. Replace it if the notes show
  it is wrong often.
- **Hermes schema drift.** `state.db` belongs to Hermes. The trace reader should fail loudly on a
  missing column, and a Hermes upgrade should re-run `trace show` on a known session.
