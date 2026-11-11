# Feature request: self-testing Personal memory

**Asked:** 2026-11-11, by the owner. **Status:** requested, not started. Feature work is frozen
(`decisions.md`, 2026-11-08) except deployment and measurement and eval tooling, so this document
says which parts count as measurement and which have to wait. The requirements as submitted are
kept verbatim in the appendix; the R-numbers used here are theirs.

## The ask, in five lines

Make Personal memory testable by itself. Every fact carries a probe (a question and its expected
answer). A weak model that has only the normal memory tools must be able to find the fact, and that
suite becomes the eval harness for memory. Two background agents then improve the memory: an
extractor that proposes new facts from the day's traces, and a reorganizer that proposes
restructures. Nothing is applied without approval, and no fact is ever lost.

## What exists today

| Requirement | Today |
| --- | --- |
| Memory store (R0) | `~/.chhaya/gateway/memory`, a local git repo with no remote. 3 files, 10 entries, 1,766 bytes. Flat by path: `interests.md`, `location.md`, `taste/food.md`. |
| Interface: read, search, propose (R0) | `memory.list`, `memory.get`, `memory.propose_update`. **No search**: `memory.search` is in `design.md` and was never built (`decisions.md`, 2026-11-08: only when a listing stops being enough). |
| Stable fact IDs (R1) | Not as a field, but every approved line ends in `<!-- proposal <id>, approved <date> -->`. That proposal id is already unique, already written by the approval path, and survives a move. It is the natural fact id. |
| Writes (R0, R4) | `ApplyApproved` appends one validated line to one document. It is the only function that writes a memory document. There is **no edit, supersede, delete or move**. |
| Approval (R3, R4) | One Telegram card per proposed line, with a rationale, tapped on the approver bot. |
| Eval runner with a weak model (R2) | Not built. The runner is step 3 and 4 of the build order in `observability-and-evals.md`, and step 3 (one non-interactive Hermes turn against an eval gateway) is still an unverified spike. |
| Golden set from real traces (R2) | Comes from the annotation page (`open-code-*.json`) and the cases that error analysis produces. The page exists; no cases yet. |
| Extractor's input (R3) | Traces exist (`evals/days/…`), per session, with tool results and, since 2026-11-11, what the model was sent. |

## Where the request meets our rules

These are not objections. They are the places where the one-pager and the repo disagree, and each
needs an answer before the build.

1. **"A resident agent owned by the memory unit" versus the gateway being the security boundary.**
   If the improver ran inside the gateway process, a model loop would sit inside the boundary that
   exists to contain model loops. It has to be a separate client of the gateway with its own
   identity (its own mTLS certificate, a new `improver` actor in the audit log) and a policy that
   allows only `memory.*`, with its own caps. Today there is one client, Hermes. A second client
   with different rules is new gateway work, with the usual blocked-case-stays-blocked tests.
2. **Restructure writes do not exist.** Move, link, re-index, supersede and forget all need new
   tool kinds with tiers, validators and audit entries (`CLAUDE.md`, non-negotiables). R1's
   "superseded fact must not surface" and R4's prune pass cannot even be expressed against an
   append-only store. Supersede is needed first, and it is a policy decision (a superseded line is
   marked, not deleted, so history stays).
3. **Approval of a restructure cannot be a Telegram card.** A diff across files does not fit one
   card. Nothing may edit Personal memory outside a propose-and-approve flow, so a restructure is a
   proposal that holds a git branch of the memory repo, reviewed as a diff with a command or the
   annotation page, and merged only by the approver path. How that approval is given is open
   question 3.
4. **The extractor reads untrusted text.** Trace content includes web pages and tool output. An
   extractor that proposes memory from them is a route for prompt injection into memory. The
   approval tap is the guard, so a proposal must show its source message and whether it came from
   the user or from a tool result. v1 of the extractor reads only messages the owner wrote.
5. **Probes are personal data.** A probe repeats the fact. They live beside the memory (same git
   repo, same backup), never in the public repo and not in `evals/` in S3 unless the owner decides
   to. Generating a probe sends the fact to a model API, as Hermes already does when it reads memory.
6. **Everything we write is Go.** The runner, probe store and gate checks are Go. The weak-model
   retriever is a Hermes run (the pinned container is the only Python), which depends on the step 3
   spike. If `--oneshot` does not load our MCP gateway, the runner calls the model API from Go
   with the memory tools as function definitions.

## What this does and does not measure

The probe suite answers: *given the memory tools, can a weak model find a fact?* That tests the
memory's structure and interface. It does not test the failure the owner has actually seen, which
is behavioural: the agent not calling memory before asking, and not finding things because it
could not list (`learnings.md`; the memory-recall-gap finding). `memory.list` addressed the second.
The first needs the error-analysis path: a trace-level check ("memory was read before the first
question") and the annotated cases. The two are complementary, and the probe runner must not be
read as a measure of how the live agent behaves.

At 10 entries in 3 files, a model that calls `memory.list` finds everything in two steps. The
structure is not the bottleneck at this size, so **the reorganizer (R4) has nothing to improve yet.**
The trigger to start it should be a measured one (open question 5).

## Proposed staging

Each stage is useful alone and stops at an approval boundary.

| Stage | What | Counts as | Needs |
| --- | --- | --- | --- |
| S0 | Fact id = the proposal id in the provenance comment. Probe schema (id, question, expected, optional expected path, kind: direct, indirect, negative, conflict). Probes stored beside the memory. Backfill for the 10 existing facts, reviewed once by the owner. | Measurement | Nothing new in the gateway. |
| S1 | Probe-bearing proposals: generating a probe with a separate model that sees the fact and not the folder structure, shown on the same approval card (R1: approving a fact approves its probe). | Feature | Freeze lifted for this; changes the card and `memory.propose_update`. |
| S2 | Eval runner: a weak-model retriever answers each probe with only `memory.list` and `memory.get` against a copy of memory in an eval gateway; scores correct, steps, tokens, path depth; reports size, duplication, max fan-out. A baseline on today's memory. Held-out golden set from annotated traces. | Measurement | The step 3 spike and the replay runner (build order 3 and 4). |
| S3 | Extractor: reads the owner's messages from a day's traces, proposes additions with probes, runs the suite first and flags contradictions. | Feature | S1, S2, the second gateway client (rule 1), and rule 4's source display. |
| S4 | Reorganizer with the four gates (deterministic fact survival, probe suite, held-out set, size and fan-out), one change at a time, as a branch. | Feature | S2, the move/supersede write kinds (rule 2), a branch approval flow (rule 3), and a measured trigger. |
| S5 | Ablations: flat, hierarchy, reorganized; with and without dedupe. Counterfactual utility (score with memory removed) as a second metric. | Measurement | S2 and S4. |

**Order I recommend:** S0, then S2, because they are measurement and give the baseline for
everything after them. S1 next, since it is small and makes the suite grow with memory. S3 only
after the suite has caught a real problem. S4 only when the trigger fires.

## Acceptance for the first two stages

- **S0:** every fact in memory has an id that matches its provenance comment, and a check fails if
  a line has no id or two lines share one. Every probe names an existing fact id; a probe whose fact
  was removed is reported. Probes never appear in the public repo (a test greps the repo's tracked
  files for the probe directory name; `memory/` is gitignored for the same reason).
- **S2:** one command runs the suite against a copy of memory, never the live store, and prints
  correct, steps and tokens per probe and in total. Run twice on the same memory it gives scores
  within the noise the runner reports (k runs per probe, as in the eval design). A deliberately broken
  copy (a fact deleted, a fact moved to a path no listing reveals) lowers the score, which is the
  test that the suite can fail.

## Open questions for the owner

1. **Probe storage.** Beside memory in the same git repo (backed up with it, no extra setup), or as
   its own data directory? I lean towards the same repo, under a dot-directory the memory tools
   cannot read, so the retriever never sees the answers.
2. **Superseding facts.** A superseded line stays in the file marked as superseded (with a pointer
   to its replacement), so history is kept and the negative probe has something to test. Is that
   acceptable, or should a superseded fact move to an archive file?
3. **Approving a restructure.** Git branch plus a `chhayactl` review command, the annotation page,
   or something else? This decides how much of S4 has to be built before it can be tried at all.
4. **Who may call the improver.** Run on a schedule on the box, or only when the owner asks. A
   schedule means the improver acts while nobody is watching; the approval gate still holds, but
   proposals could pile up unread.
5. **The trigger for S4.** Suggested: start when the probe suite shows steps rising, or when any
   file or folder passes a size or fan-out threshold chosen from the S2 baseline, not guessed now.
6. **Probe model.** A different model family from the retriever and the generator, as R1 asks, costs
   a second provider key on the box. A different model of the same family is cheaper and weaker as
   a guard against circular probes. Which?

## Risks not in the one-pager

- **Memory poisoning through the extractor** (rule 4).
- **Approval fatigue.** S1 and S3 add content to every card and more cards. If taps become
  automatic the gate stops being a gate; the proposal count per day should be capped and reported.
- **The probe suite as a new place for personal data**, backed up and snapshotted like memory.
- **Goodhart on the suite from the other side.** A weak retriever tuned on 10 facts will be perfect
  and prove nothing. The suite means something only once memory is large enough to fail.

## Out of scope

Everything in the one-pager's non-goals, plus building any of it before the owner lifts the freeze
for S1 and later.

## Appendix: requirements as submitted (2026-11-11)

## Self-Testing Memory for Chhaya: Requirements (one-pager)

### Context
Chhaya's personal memory is a hierarchical file store (`/profile.md`, `/areas/`, `/topics/`, `/people/`), read and written through one memory interface with human approval on writes. Goal: make the memory **self-testable**, so evals fall out of the memory itself, and let two background agents (extractor, reorganizer) improve it safely.

### Core idea
Memory is a **self-contained unit that owns its own agent**, and that agent's sole goal is to improve the memory. The unit bundles the facts, their probes (evals), the interface, and a resident improver agent. Anything else (the assistant, the CLI, a UI) only talks to the unit through its interface and approval flow. The unit can test itself, so it can be improved without touching the rest of the system.

The unit is self-testable because every fact carries a **probe**: a question plus expected answer. A low-reasoning agent (Haiku-class) using only the normal memory interface must be able to find the fact. If it can, the structure is navigable. The probe suite is the eval harness for everything below.

### Requirements

#### R0. Memory as a self-contained unit
- One module owns: fact store, probes, eval runner, interface, and the memory-improver agent (extractor and reorganizer are its two modes).
- External agents get only the interface: read, search, propose-write. All writes go through approval.
- The improver agent's objective is explicit and measurable: raise probe and golden-set scores and cut retrieval cost, with zero fact loss.
- The unit can be run, evaluated and rolled back on its own, independent of Chhaya or any specific assistant.

#### R1. Probes stored with facts
- Each fact has a stable ID and 1-2 probes (question, expected answer, optional expected path).
- Probes are generated by a separate model that sees the fact but NOT the folder structure.
- Include direct probes ("favourite movie?") and indirect ones ("what should I watch Friday?").
- Include negative probes (superseded fact must not surface; absent fact returns nothing) and conflict/update probes.
- Approving a fact approves its probe. The suite grows with memory.

#### R2. Eval runner
- A weak-model retriever answers each probe using only the real memory tools.
- Score: correct (yes/no), steps, tokens, path depth.
- Also report memory size, duplication, and max fan-out per folder or index file.
- Maintain a **held-out golden set** built from real traces and daily labels. The reorganizer never sees it.

#### R3. Extractor agent
- Reads a day's session traces and proposes additions via the same memory interface and the same approval flow.
- Each proposal ships with its probe(s) and is shown as a diff.
- Before proposing, run the existing suite and flag any new fact that contradicts or shadows existing facts.
- Respect existing privacy rules on what may be stored.

#### R4. Reorganizer agent
- Proposes restructures as a diff or git branch. Nothing is applied without approval.
- **Lossless by construction:** fact lines are immutable; it may only move, link and re-index. Merging or rewording is a separate gated operation.
- Order of passes: dedupe, then restructure, then prune/forget, each evaluated separately.
- Surface a proposal only if all gates pass and cost or steps improve:
  1. Deterministic gate: every fact ID, number, name and date survives.
  2. Probe suite does not regress.
  3. Held-out golden set does not regress.
  4. Size, duplication and max fan-out do not worsen.
- Change one thing at a time so regressions can be attributed.

#### R5. Versioning
- Memory lives in git (or equivalent). Every agent change is a commit with rollback.

### Non-goals
- No learned or RL consolidator in v1.
- No vector DB; stay with the file hierarchy.

### Risks to design against
- **Circular evals:** probes lexically mirror the fact. Mitigate with indirect probes written by a different model, plus the held-out golden set.
- **Goodhart:** reorganizer games the probes (duplicating facts, stuffing index files). Mitigate with cost/duplication metrics and the held-out set.
- **Hub files:** catch-all files or folders become unnavigable. Track fan-out.

### Prior work to borrow from
- Filesystem-based memory for LLM agents (alphaxiv 2607.26637): management/search/execution roles over one memory filesystem.
- Letta MemFS and sleep-time agents: git-backed memory, background "dream" subagents.
- MIRIX-2.0 (arXiv 2611.12068): complete records as source of truth, lossless AutoDream merge (never destroys stored memory), union-coverage gate, ablation template (flat vs graph vs graph + dreaming).
- Auto-Dreamer (2605.20616): counterfactual utility (performance drop when memory is removed) as a second metric.
- LongMemEval: probe taxonomy (extraction, multi-session, temporal, knowledge update, abstention).

### Suggested build order
1. Fact IDs and the probe schema.
2. Eval runner with the weak-model retriever; baseline on the current memory.
3. Extractor with probe-bearing proposals.
4. Reorganizer with the gates in R4.
5. Ablations: flat vs current hierarchy vs reorganized, with and without the dedupe pass.
