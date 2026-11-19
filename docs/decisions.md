# Decisions

Append-only. One entry per choice, newest last. The point is that later sessions don't re-litigate: if a decision is here, it's settled until new evidence shows up, and the evidence that would reopen it is written down.

---

## 2026-10-20 — Project name is `chhaya`

The design doc called the repo `pa/`. Renamed throughout: `chhaya-gateway`, `CHHAYA_CONFIG`, `mcp_servers.chhaya`, `chhaya-approver`, `~/code/chhaya`.

**Why:** the directory already existed under that name and `pa` is too generic to live in config keys and tool prefixes we'll read daily.

---

## 2026-10-20 — The kill switch lives on the approver bot

`/stop` and `/resume` are commands on **chhaya-approver**, handled by the gateway directly. Not on Hermes' Telegram bot.

**Why:** `/stop` sent to Hermes arrives through the process it is meant to stop. A confused or compromised agent is exactly the case the kill switch exists for, and exactly the case where it cannot be trusted to relay the command. Same reasoning that put approvals on a separate bot.

**Backstops, in descending order of trust:** a flag file the gateway checks before every write; stopping the gateway container, which removes Hermes' only route to code, money and data. The switch fails closed — if the gateway cannot read its own state, it refuses writes.

**Reopen if:** we ever collapse to a single bot for UX reasons. Don't.

---

## 2026-10-20 — The gateway is a standalone HTTP service, not a stdio subprocess

Hermes connects to the gateway over HTTP/SSE with mTLS. It does not spawn it.

**Why:** three findings from the Hermes docs, any one of which is sufficient.

1. A stdio MCP server is a **child process of Hermes**, so Hermes would own the lifecycle of the process holding the kill switch.
2. Hermes **strips credential-named env vars** from MCP subprocess environments, so the gateway's Swiggy, GitHub and Garmin secrets would have to be declared in Hermes' own config `env:` block — putting every credential inside the one component designed never to hold any.
3. The gateway would die with Hermes, and cron jobs with it.

**Cost:** slightly more M0 setup (certificates, a second container). **Benefit:** the AWS move is two containers in one compose file rather than a re-architecture.

**Also settled here:** sampling and elicitation are disabled on the MCP server. Both default to **on** in Hermes. Elicitation would surface approval prompts as Telegram buttons *through the agent* — the exact thing the separate approver bot exists to avoid.

> **Syntax corrected by the M0a spike entry below.** This entry originally gave the form as `sampling: false` / `elicitation: false`. That is wrong and crashes at startup; the real shape is `sampling: {enabled: false}`.

---

## 2026-10-20 — `skills.write_approval: true` is permanent, not "at first"

The first draft treated this as early-days caution to relax later.

**Why it changed:** a Hermes skill that declares `required_environment_variables` in its frontmatter gets those variables forwarded into the sandbox **automatically, with no config change**. Hermes authors its own skills. That is a path from "the agent wrote a skill" to "a secret is in the sandbox" that never crosses my desk. Skill write approval is what closes it.

Related: `HERMES_WRITE_SAFE_ROOT` is set, and `terminal.env_passthrough` is explicitly empty alongside `docker_forward_env`.

> **Strengthened by the M0a spike entry below.** `env_passthrough: []` turns out **not** to block skill-declared passthrough — those variables are forwarded automatically regardless. Skill write approval is the only control, with nothing behind it.

---

## 2026-10-20 — Trace correlation uses `session_affinity_header`; no fork needed

Hermes custom providers accept `session_affinity_header`, which carries the conversation id on every request to that endpoint — main turn and auxiliary calls (compression, titles, background review). The proxy, the gateway and the session store all key on the same id.

**Consequence:** the header carries a *conversation* id, not a task id, and a Telegram chat is one long conversation. "One task = one trace" therefore depends on `/new` at task boundaries. `/new` was already the rule for refreshing memory; it is now also what keeps traces addressable.

**Fallback if the discipline slips:** segment sessions into tasks at the gateway. Cheaper first move: have the evening digest nudge when a session has run long.

---

## 2026-10-20 — Personal memory stays a gateway tool, never a Hermes plugin

**Why:** Hermes does support external memory providers as plugins (Mem0, Honcho, Hindsight and others), so this was a live option. But a plugin runs *inside* Hermes — inside the boundary we deliberately don't trust. Using it would put the model of me, the asset, on the wrong side of the gateway.

This closes an open question from the first draft.

---

## 2026-10-20 — Arm A of the ablation becomes a real competitor

The success test was "beats a vanilla chat agent running the same model with the same tools."

**Why it changed:** Muse and Instinct launched in September 2026. For consumer errands they are free and good, so "beats a vanilla chat agent" flatters us on exactly the job where we are least differentiated. Arm A is now the best general agent available for the task — Muse, where Muse covers it.

**Expected result, stated in advance so we can't rationalise later:** we lose on food, win on code, health and anything that must stay private. If we also lose on code and health, the project isn't earning its keep.

---

## 2026-10-20 — Swiggy MCP is real; access is the risk, not capability

Four official servers at `mcp.swiggy.com`. Food is 20 tools at `/food`, remote JSON-RPC over streamable HTTP, OAuth 2.1 + PKCE, session-based auth, no raw card details reach the agent.

**Two consequences:**
- **No native spending cap** is documented, so caps stay ours — as the design already assumed.
- **Production access is invite-based**, requiring whitelist onboarding and a partner agreement. Unknown lead time, and M3 depends on it. Apply early; let M3 slip behind M1 and M2 rather than block them.

The **gateway**, not Hermes, is the OAuth 2.1 PKCE client.

---

## 2026-10-20 — Pin Hermes to a tag, never track `main`

Running v0.22.2 (published Oct 15 2026).

**Why, concretely:** v0.22.1 was built from the wrong commit and shipped unreleased gateway functionality; v0.22.2 is the correction. The repo also carries ~50k open issues. The first draft's "pin a release" instinct was right and this is the evidence for it.

---

## 2026-10-20 — M0a spike: Hermes v0.22.2 verified against source

Cloned `v0.22.2` (`d923a5e`) and read the config handling directly rather than trusting the docs. Four findings, two of which would have broken M0.

**1. `sampling` / `elicitation` are nested dicts, not booleans.** From `tools/mcp_tool_server_run.py:268-276`:

```python
sampling_config = config.get("sampling", {})
self._sampling = (... if sampling_config.get("enabled", True) and ... else None)
```

The shape the design doc had inferred (`sampling: false`) would make `config.get("sampling", {})` return `False` and then raise `AttributeError` on `.get()` — a **startup crash**, not an ignored key. Correct form is `sampling: {enabled: false}`. The `("enabled", True)` default also confirms both features are on unless disabled.

**2. `env_passthrough: []` does not close the skill-secret path.** From `hermes_cli/config_defaults.py:326-328`:

```python
# Env vars passed into sandboxed terminal/execute_code (skill-declared
# required_environment_variables pass through automatically).
"env_passthrough": [],
```

A skill that declares `required_environment_variables` gets them forwarded **whether or not** `env_passthrough` is empty. So the empty list is not a second layer; `skills.write_approval: true` is the only control, and it is load-bearing exactly as the earlier entry claimed — more so, since there is no backstop behind it.

**3. `approvals.mode` defaults to `smart`**, not manual: an auxiliary LLM auto-approves what it judges low-risk. Our `manual` is a deliberate move from "a model decides" to "I decide". Also present and undocumented in the config reference: `approvals.deny` (fnmatch globs that block even under `--yolo`), `smart_policy`, `denial_breaker_threshold`.

**4. Confirmed as documented:** `session_affinity_header` is real (`agent/opencode_affinity.py`) and is merged in `agent/auxiliary_client.py` too, so auxiliary calls carry it as claimed. `docker_forward_env: []` and `HERMES_WRITE_SAFE_ROOT` (os.pathsep-separated) are real. `cron_mode`, `single_query_mode`, `unattended_mode` all exist and already default to `deny`. `client_cert` + `client_key` is a valid pair.

**Lesson for the rest of the project:** the Hermes docs are good but not exact, and this repo is MIT Python sitting on disk. For anything security-relevant, read the source. Two of the four findings above were invisible from the docs.

**Still open from this spike:** live mTLS connection, `session_affinity_header` observed arriving at a real proxy, the two-bot Telegram shape, and docker-backend secret isolation. Those need a running Hermes.

---

## 2026-10-21 — M0a spike: runtime verification, and Hermes cannot be run from source here

Second half of the spike. Stood up a real MCP server over mutual TLS and exercised Hermes' own code against it.

**Verified working**

- **mTLS MCP transport.** Full `initialize` → `tools/list` → `tools/call` against an HTTPS MCP server with `ssl.CERT_REQUIRED`. The server logged `client_cert_cn='hermes-client'` on every request. Negative control — same client, no certificate — was rejected at the TLS layer (`certificate required` / `httpx2.ReadError`). Hermes' own `_resolve_client_cert` turns our `client_cert` + `client_key` config into the correct httpx tuple. **The standalone-gateway decision is sound at the transport level.**
- **`session_affinity_header`.** Using Hermes' own functions against a real `~/.hermes/config.yaml`: the header name resolves from the provider entry, `custom_provider_session_affinity_headers` stamps it with the session id, and an endpoint that did not opt in gets `{}`. Trace correlation works as designed, with no fork.

> **Corrected by the Docker-image entry below.** The headline of this section was wrong: Hermes does **not** pin a Python alpha. It pins **3.14.7 stable** (and uv 0.12.3) in `pm/lock.json`. Our local uv 0.7.3 was too old to know 3.14.7 and resolved `.python-version: 3.14` to the only 3.14 build it had — the alpha. The segfault was our stale toolchain, not a Hermes defect. The operational conclusions below (use the image, isolate the gateway's runtime) still hold, for better reasons.

**Hermes v0.22.2 cannot be run from a source checkout on this machine**

`.python-version` pins **3.14**, and `uv sync` discards any venv you made and rebuilds on **3.14.0a6** — an alpha. On that interpreter:

```
import charset_normalizer   →  Segmentation fault (exit 139)
```

Same wheel (3.5.2) on stable 3.12.10 imports fine. `requests` imports `charset_normalizer`, so any Hermes path touching `requests` dies — which includes the model-catalog import chain reached from ordinary config lookups.

**Consequences**

1. **Run Hermes from its official Docker image, not a source checkout.** The image pins a combination its authors test; our reconstruction via `uv sync` is not that. The design already containerises Hermes, so this costs nothing — it just makes the container mandatory rather than tidy.
2. **The gateway never shares an interpreter, venv or image with Hermes.** Already true process-wise from the standalone-service decision; this extends it to the runtime. Our code targets stable Python 3.12.
3. Source checkouts stay useful for *reading* — which is how the two config errors were found — just not for running.

**Not claimed:** that Hermes is broken for everyone. This is one Linux x86_64 reconstruction of their lock. The official image may ship a working build; we have not tested it.

**Incidental, but it will bite someone**

The MCP Python SDK has moved on: `streamablehttp_client` → `streamable_http_client`, it now takes `http_client=` built on **`httpx2`** (not `httpx`), and result fields are snake_case (`server_info`, not `serverInfo`). Hermes imports the SDK lazily via `_ensure_mcp_sdk`, so it is not in the base dependency set. Relevant when the gateway acts as an MCP *client* to Swiggy.

---

## 2026-10-21 — Official Docker image verified; the alpha finding was my error

Pulled `nousresearch/hermes-agent` v0.22.2 by digest (`sha256:a11574b7…`) and re-ran everything that failed from source.

**Correcting yesterday's finding.** I wrote that Hermes pins a Python alpha. It does not. `pm/lock.json` — which the Dockerfile calls the "single authority" — pins **CPython 3.14.7+20260901 (stable)** and **uv 0.12.3**. Our local **uv 0.7.3** predates 3.14.7 and knows only one 3.14 build, `3.14.0a6`, so it resolved `.python-version: 3.14` to the alpha and built the venv on it. The `charset_normalizer` segfault was a consequence of our stale toolchain, not a Hermes defect. Their Dockerfile comment anticipates exactly this class of mistake ("that is exactly the two-authorities failure the pm design exists to end"), and we walked into it.

**The image works.** Python **3.14.7**; `charset_normalizer` 3.5.1 and `requests` 2.33.0 import cleanly; and all four `session_affinity_header` checks pass *inside the real runtime*, including `merge_session_affinity_headers`, which was the call that segfaulted:

```
1 header name resolved : 'x-chhaya-session-id'
2 stamped with session : {'x-chhaya-session-id': 'sess-abc123'}
3 not leaked elsewhere : {}
4 merged into kwargs   : {'extra_headers': {'x-chhaya-session-id': 'sess-abc123'}}
```

That is exactly what our proxy will receive. Trace correlation is verified end to end on the runtime we will actually deploy.

**Decisions this settles**

- **Run Hermes from the official image, pinned by digest, not by tag.** Tags move fast: v0.22.2 shipped Oct 15, **v0.22.3 on Oct 21**, and `rc.1-v0.22.4` the same day — two releases in six days, with `latest` and `stable` both already moved. A tag pin is not a pin.
- **If we ever build from source, use their `pm` provisioner and its pinned uv — never whatever `uv` is on `PATH`.** That is the lesson of the alpha, and it generalises: for Hermes, the lock file is the authority, not the tooling we happen to have.
- The gateway still gets its own runtime and image. Unchanged, but note the reason has shifted from "Hermes ships an alpha" (false) to "Hermes pins an aggressive, fast-moving toolchain we should not inherit" (true).

**Three operational facts for `deploy/`**

1. **`HERMES_HOME` must be writable.** Hermes creates `cron/` and other subdirectories at init; a read-only mount fails with `HomeInitializationError`. The data volume cannot be mounted `:ro`.
2. **`HERMES_WRITE_SAFE_ROOT=/opt/data` is set by default in the image** and resolves correctly (`get_safe_write_roots() -> ['/opt/data']`). The docs' claim is verified; we do not need to set it ourselves when using the image.
3. **Overriding `--entrypoint` bypasses the privilege drop.** The image declares `USER root` and relies on an s6-overlay stage2 hook to remap to uid 10000 at boot. `docker run --entrypoint sh` lands you at `uid=0(root)`. Our compose must use the real entrypoint, or pass `--user` explicitly — and any debugging shell we open is more privileged than the running agent, which is worth remembering before concluding "the sandbox lets me do X".

---

## 2026-10-21 — M0a spike closed: the two-bot approval channel works

Live test against the real bots. Hermes is `@satish_chhaya_bot` (id 8979753688); approvals and `/stop` ride on `@satish_chhaya_approver_bot` (id 8940896193). Distinct bots, distinct tokens, distinct env files, neither file containing the other's secret. Both have group-join disabled and privacy mode on, so neither can be added to a group or read group traffic.

**Verified end to end:** the approver bot sent an approval card with ✅/❌ inline buttons, the tap came back as a `callback_query` carrying `data=spike:approve` and `from.id` matching the configured allowlist, and `answerCallbackQuery` closed the loop with a toast. That is the exact mechanism tier-2 approvals will use, on a chat Hermes has no token for.

**A failure mode that would have been invisible in production**

A Telegram bot **cannot initiate a conversation** — the user must message it first. Before `/start`, `sendMessage` to the approver bot returns `Bad Request: chat not found`.

Deployed without that step, every approval request would vanish and every tier-2 action would time out into a denial. The system fails closed, which is right, but *silently*, which is not: the symptom is "the agent won't order food" with no stated cause.

**Therefore: the gateway probes the approver chat at startup and refuses to start if it cannot reach me.** A safety channel that is merely unreachable must be loud, not quiet. This is the inverse of the kill switch — that one fails closed and silent on purpose; this one fails closed and noisy. Both are "refuse to act", but only one is a configuration error worth shouting about.

**Operational notes**

- `getUpdates` is destructive-ish: updates stay queued until the offset is advanced past them. Both queues were drained after the test so the gateway is not handed stale approvals on first boot. Long polling and a one-shot `getUpdates` compete for the same queue — only one consumer per bot token, ever. This is an argument for the gateway owning the approver token exclusively, which it already does.
- The numeric user id (`8726466304`) was confirmed against both bots independently rather than trusted from a third-party lookup bot.

**Spike complete.** Every M0a question is answered: MCP over mTLS, trace correlation via `session_affinity_header`, the four config keys, the official image as the runtime, and the two-bot approval channel. Four corrections to the design came out of it, two of which would have broken M0 on first run, plus one error of my own (the Python "alpha" claim) caught and reversed.

---

## 2026-10-21 — Pin Hermes v0.22.3 by digest; verification is now a script

Re-verified against **v0.22.3** (published Oct 21, same day I pinned v0.22.2) and moved the pin.

```
nousresearch/hermes-agent@sha256:1d33f6087fab79d580ffaec34ceed825e11d7bb8e803206be75a56d027c62538
```

All seven assumptions hold on both v0.22.2 and v0.22.3: stable interpreter (3.14.7), clean `requests`/`charset_normalizer` import, session-affinity stamping with no leak to other endpoints, `sampling`/`elicitation` still nested dicts defaulting to on, skill-declared env vars still auto-forwarded, `approvals.mode` still defaulting to `smart`, and `HERMES_WRITE_SAFE_ROOT=/opt/data`.

**The check is now `scripts/verify-hermes-image.sh`, not a memory.** Hermes ships every few days; "I checked this once" has a short shelf life, and the assumptions it protects are the ones holding up the safety model. Run it against any new digest before moving the pin. Each check names the decision it defends, so a failure says what to go re-read rather than just going red.

**It caught something on its first real run — and the something was my own check.** The skill-env-passthrough test string-matched a comment in `config_defaults.py`. Between 0.22.2 and 0.22.3 the comment was reworded from *"skill-declared required_environment_variables pass through automatically"* to *"plus skill-declared required_environment_variables"*. Identical meaning, failed test. The check now asserts on `tools/env_passthrough.py`, the actual implementation, instead of prose.

Worth keeping as a rule for the eval suite we are about to write: **assert on behaviour or structure, never on wording.** A test that fails when a comment is rephrased trains you to ignore it, and a safety test you have learned to ignore is worse than no test.

**New in 0.22.3, noted not adopted:** `terminal.client_env_passthrough` — "extra non-secret toolchain names a CLI/TUI client sends". A separate, narrower surface than `env_passthrough`, and explicitly non-secret, but it is a third way for variables to enter the sandbox. Leave unset; revisit if we ever run the TUI client.

## 2026-10-22 — Go is the implementation language; Python exists only as Hermes

**Decision.** Everything we write is Go. Hermes is the only Python in the system, and we run it as a pinned container, never as a library.

**Why it needed writing down.** `design.md` and `CLAUDE.md` were drafted before this constraint and described a Python gateway (`gateway/server.py`, `tools/memory.py`, "Python 3.11+, `uv`, `pytest`"). The language choice is not cosmetic here: the bypass-proof invariant in the M0b spec depends on Go's unexported-identifier rule to make the tool registry unreachable except through `Invoke`. In Python that invariant would be a review convention rather than a compiler-enforced property.

**Consequence worth remembering.** A capability that exists only as a Python library does not get linked into anything we write; it gets its own process behind a gateway tool. This already bites the Garmin open question — the usable unofficial clients are Python, so "start unofficial" means a separate sync process, not an import. That cost belongs in the M1 decision, not discovered during it.

**What would reopen it.** Nothing short of changing the stack decision itself. Scala was the other candidate and was not chosen.

---

## 2026-10-22 — Garmin starts unofficial, as a Python sidecar; M1 ports `health-assistant`

**Decision.** M1 reads Garmin through the unofficial route (`garminconnect`/`garth`) running as a **separate Python sidecar process** that posts into a Go store the gateway reads. We do not wait for the official developer program, and we do not link a Python library into anything we write. This closes the open question in `design.md` that was marked **Blocks M1**.

**Why now.** We already own working code for this: `~/codebase/health-assistant` has run exactly this architecture since January 2026 — Python sidecar → HTTP → Go validate/transform/store. The question was never "is the unofficial route viable", it was "what does it cost us". The answer is a second process and a scraper that can break, both of which we had already accepted in the risk table ("Unofficial Garmin access breaks → sync into our own store; coach degrades gracefully on stale data").

**What this buys.** About 750 lines of Go and 350 of Python we don't write: the six-data-type field mappings, the normalised structs, the validators with their tests, the payload→event transforms, three correlation functions that are the content of the daily nudge, and a sync-audit record that maps field-for-field onto the L0 ops eval. The inventory, with what to leave and what not to carry over, is in `docs/m1-garmin-reuse.md`.

**What we deliberately don't take.** TimescaleDB (SQLite for one user), the `user_id` plumbing (single user), JWT/Google Sign-In and the dashboard REST layer (no app, no second user, mTLS instead).

**One finding worth keeping.** The sync in that repo has been failing since 2026-02-18: an ingest-secret middleware was added to the six Garmin routes and the Python client was never updated to send the header, so every POST 401s. It went unnoticed for eight months because nothing tested the real boundary. **For chhaya: the sidecar's authentication to the gateway needs a test that crosses the process boundary, not a handler test.** The same gap would be invisible in unit tests here too.

**Reopen if:** Garmin's web endpoints change in a way garth can't track, or the official program becomes available without a commercial agreement. Either way the sidecar is the thing that changes; the store, the tools and the coach logic don't.

---

## 2026-10-23 — M0b is implemented; what the build confirmed and what it changed

Ten tasks landed the policy gateway: `internal/policy` (classifier, evaluation order, caps, taint, kill switch), `internal/state` (durable), `internal/audit` (hash-chained log), `internal/approval` (the Telegram approver bot), `internal/mcpsrv` (the one exported door to a tool handler, over HTTPS+mTLS), `internal/tools` (memory, capabilities, ping), and `cmd/chhayad`/`cmd/chhayactl`. The exit check passes: `/stop` → `memory.propose_update` → `Deny(killswitch.active)` → an audit record → `chhayactl audit verify` all run end to end.

**Confirmed.** The evaluation order survives contact with the SDK: `github.com/modelcontextprotocol/go-sdk`'s `mcp.Server.AddTool` hands every tool's handler the same `req.Params.Arguments` (raw `json.RawMessage`), so one handler shape — unmarshal, call `Invoke`, marshal — covers every tool, and no tool gets its own undocumented path to its handler. The bypass-proof property the spec asked for held without a workaround.

**Changed: the Go version.** `go.mod` says `go 1.25.0`, not `go 1.24` as the spec stated. `github.com/modelcontextprotocol/go-sdk@v1.8.0` declares `go >= 1.25.0`, and the installed 1.24.3 toolchain built it anyway by auto-downloading 1.25.0. Nothing to act on; worth knowing before assuming the toolchain on a fresh machine will build this module unattended.

**Added beyond the spec.** `internal/state` is a whole package the spec didn't call for, because `/stop` has to survive a restart and `internal/policy` has to stay pure — something has to own the state file on disk, and it cannot be the package that is supposed to be a pure function of its inputs. `policy.Request` also grew past its spec shape: `Known`, `ArgsOK`/`ArgsError`, `ValidatorOK`/`ValidatorErr`, and `CapKey`/`AmountMinor` all got added as the evaluation order was built out. Four rules — unknown tool, unparseable arguments, a failing validator, and the spending caps — have no way to reach `Decide` without one of these fields. A fifth rule, the kill switch, needed no new field at all: it reads `State.KillSwitch` directly and was already reachable. The spec is explicit that the order *is* the security property and is tested there, so the four added fields aren't incidental plumbing; without them, those rules would have had to live as checks bolted on somewhere else, which is exactly the "second path to a tool" shape the design is built to rule out.

**What review caught that the plan had wrong.** Several of these would have been real holes if they'd shipped as planned, and the record is worth keeping unflattering:

- The `make evals` target ran `go test ./internal/policy/... -run 'TestDecide' -v`. The `-run` filter silently skipped the four property tests asserting the named safety invariants — "evals passing" would have meant nothing was actually checked.
- The audit log's redaction marker is `<redacted:NB>`, which contains the two characters `json.Marshal` HTML-escapes by default. Hashing with one encoder and writing with another meant the plan's own round-trip test could not pass against the plan's own code; the fix was one canonical encoding (`SetEscapeHTML(false)`) used for hashing, writing and verifying alike.
- `json.Unmarshal` of the literal `null` into a struct target succeeds and leaves it zeroed. A state file corrupted down to four bytes (`null`) would have read as "kill switch off" — the exact inverse of what the file exists to guarantee. Fixed by decoding into a pointer, checking for `nil`, and requiring a version field.
- `dispatch` set the taint flag only after an untrusted handler returned, so an untrusted tool's *error string* — equally untrusted content — came back unmarked. Taint is now set before the handler runs, not after.
- The memory path's containment check resolved symlinks on the full path, which skipped the check entirely whenever the final path component didn't exist yet — precisely the case a write hits. Fixed by validating the parent directory first, then any existing final component separately.
- The planned approval card sent the card text and the inline buttons as two separate Telegram messages, minting the callback token from the first message's id while attaching the buttons to the second. `tap()` would have compared against the wrong id and every real approval would have read "that card has expired." Caught in a pre-flight review before the task that would have shipped it ran; fixed by sending one message and attaching the keyboard to it once its id exists.
- `MCPHandler` refused a call with malformed arguments before ever reaching `Invoke`, which meant it left no audit record — breaking `Invoke`'s own contract that every call runs `policy.Decide` → dispatch-or-refuse → `audit.Append`, no exceptions. Fixed by routing `ArgsError` through `policy.Request` as a new rule (`args.unparseable`) inside the same evaluation order, rather than a second refusal path outside it.

**Five items deferred, each with the evidence that reopens it.**

- **Per-session taint** stays coarse (gateway-wide, not per conversation) until the trace proxy lands and the gateway can tell which session a call belongs to.
- **Spending caps don't fire yet.** `AmountMinor` and `state.RecordSpend` exist on the data model but aren't wired to a real tool, because there is no tier-2 tool to wire them to. Until then, `buildRegistry` refuses tier-2 registrations outright rather than register a cap that nothing enforces. Reopens with the first money tool.
- **A dead approval channel still does not auto-refuse writes.** `internal/approval` detects and loudly audits a sustained outage (CRITICAL record after ~2 minutes of failed polling), but a tier-1 write attempted during that outage is not itself blocked by the outage alone — there is no tier-1 tool yet for this to matter to. Reopens with the first tier-1 tool.
- **The audit log has no external anchor, so tail truncation is undetectable.** The hash chain catches an edit, a removal or a reorder *inside* the log, because every later record's `prev` stops matching. Dropping the last n records does not break anything: what remains is a shorter, internally consistent chain, `Verify` reports it clean, and `Open` resumes appending at the truncated sequence. Catching that needs the last seq and hash recorded somewhere the attacker does not also control — `state.json` is the obvious place, and the gateway already writes it. This was found by a whole-branch review, and the response was to correct the claim rather than build the anchor: the package doc and spec §8 now say what is actually guaranteed, and `TestTruncationFailsVerification` was renamed to `TestARemovedRecordFailsVerification`, which is what it tested all along. Reopens when the audit log needs to survive an attacker with filesystem access.
- **Denied calls do not count toward the rate limit.** `state.RecordCall` is called in `dispatch`, which a denied call never reaches, so an agent can be refused by the same rule without limit. The deferral stands for M0b because all four tools are harmless and nothing downstream is touched. Be clear that a denied call is **not** free, though: it is an fsync'd append to a log that is never rotated, so a loop on a denied tool is an unthrottled disk-write amplifier aimed at the one file the whole safety model rests on. Combined with the audit-unwritable latch added in the same review, a full disk now means a gateway that refuses everything rather than one that serves unlogged — a safe failure, but still a denial of service an agent can cause by itself. Reopens before the first real workload, i.e. with the first tier-1 tool.

**Documentation debt closed alongside the code.** `design.md` now states why Hermes' web/browser/fetch toolsets are disabled (the gateway can't taint-track a read it never sees) rather than listing it as a preference, and `scripts/verify-hermes-image.sh`'s three structural config-key checks (sampling/elicitation shape, skill-env-passthrough, approvals.mode) are `grep` against the image instead of `inspect.getsource` calls. Python survives in four `python3` invocations, and it is worth being precise about why, because the rule is "assert on behaviour or structure, never on wording" and three of these assert on behaviour. Two execute Hermes' own code: the session-affinity check imports `agent.opencode_affinity` and the write-sandbox check calls `agent.file_safety.get_safe_write_roots()`. A third exercises the image's interpreter and its third-party dependencies rather than Hermes' own modules — the `requests`/`charset_normalizer` import, which is what caught the 3.14.0a6 segfault. The fourth is a bare interpreter-version probe that runs no code at all.

---

## 2026-10-23 — What the whole-branch review of M0b caught that per-task review could not

Eleven per-task reviews and twelve fix rounds had already run. Everything below lived in a seam between two packages or two processes, which is the shape a per-task pass structurally cannot see.

**The state file is the source of truth, not a Store's copy of it.** `state.Store` read the file once in `Open` and answered every later `Snapshot` from memory. `chhayad` and `chhayactl` are different processes with different Stores, so `chhayactl stop` was invisible to a running daemon, and the daemon's next `RecordCall` flushed its stale copy back over the operator's kill switch — while the CLI had already printed "kill switch ON". The scenario `chhayactl` exists for, a daemon whose Telegram poll loop is dead, is exactly where it was a no-op. `Snapshot` and `update` now both read the file; `update` is read-modify-write. The cost is a few-hundred-byte read per tool call, from page cache. Deliberately no mtime or size shortcut: a second write inside the same timestamp granularity is precisely the one we must not miss. `Snapshot` still returns no error, so an unreadable file fails closed with the kill switch on; a missing file stays a legitimate fresh start.

**An audit append that fails at runtime now latches and refuses everything.** Startup already refuses to open a log it cannot verify. At runtime the same condition printed one line to stderr and kept dispatching, so a full disk or a detached volume meant a gateway serving unlogged, unattended. The first failed append now sets a flag that refuses every later call under `audit.unwritable` — reads included, because a read we cannot log is an answer we cannot prove. The dispatch that discovered the failure still completes; that cannot be undone, only the next one prevented.

**`tools.All` was an exported path to every handler.** The registry is unexported inside `mcpsrv`, but `tools.All(d)[2].Handle(ctx, args)` reached `memory.propose_update` from anywhere in the module with no `Decide` and no `Append`. The spec's "no exported function reaches a handler directly" was true of the package and false of the module, so the invariant test now walks the whole module and allows a `.Handle(` call only in `mcpsrv.dispatch` and in `internal/tools`' own tests. It tells `http.ServeMux.Handle` apart by resolving the receiver, and reports any receiver it cannot prove.

**The approval card was safe from HTML injection by accident.** `SendMessage` posts with `parse_mode: HTML` and the card interpolates agent-controlled values. What saved it was `json.Marshal`'s default HTML escaping — while `audit.MarshalArgs` deliberately disables exactly that, because the bytes written must be the bytes hashed. A reviewer asking for one marshaller would have handed the agent the ability to close a `<code>` and forge a `tool:` line over a human's approval. `card()` now escapes every interpolated value itself and depends on no marshaller's behaviour; both marshallers carry a comment saying why they must stay different.

**Claims, not just code.** `TestTruncationFailsVerification` was named for the general case and only tested removal from the middle. A test named for a property it does not test is worse than no test, so it is now `TestARemovedRecordFailsVerification` and the package doc says what a bare hash chain actually guarantees. Separately, `readAll` used `json.Unmarshal`, which drops unknown keys — a field injected into a record survived verification untouched. Now `json.Decoder` with `DisallowUnknownFields`.

**The gateway container could not write its own data directory.** `/data` did not exist in the image, so Docker created the named volume `root:root` while the container runs as uid 10001: `audit.Open` fails on `O_CREATE`, `chhayad` calls `log.Fatalf`, and `restart: unless-stopped` turns that into a crash loop. Verified both ways by building the image before and after. The image now creates `/data` owned by 10001 so a fresh volume is seeded with that ownership.

**The pattern worth keeping.** Four of these six are the same mistake in different clothes: a correct in-process invariant assumed to hold across a process, a file, a container boundary or a future refactor. Per-task review cannot see that, because each task was right about the thing it owned.

---

## 2026-10-24 — The gateway could not read its own TLS key, for the same reason it could not write its own data directory

The entry above records `/data` being created `root:root` under a container running as
uid 10001. This is the other half of that bug, found the first time the stack was started
for real: `deploy/gen-certs.sh` ended with `chmod 600 ./*.key`, leaving `gateway.key`
owned by whoever ran it — uid 1000 on a developer machine — while `Dockerfile.gateway`
runs the gateway as uid 10001. Bind mounts do not remap ownership, so the key arrived
inside the container as `1000:1000 0600` and `chhayad` died on
`load server keypair: permission denied`, which `restart: unless-stopped` turned into a
crash loop. Two files each held half the fact and neither referenced the other.

**The fix is on the host, at generation time.** Nothing inside the container can correct
it: the mount is `:ro` and there is no root phase to chown from. `gen-certs.sh` now grants
the gateway's uid read on `gateway.key` and traverse on the certs directory, and **reads
that uid out of `Dockerfile.gateway`** rather than writing it down a second time — the two
drifting apart is the entire bug, so the number now lives in one place.

**An ACL, not chmod or chgrp.** `chmod 0644` makes a private key readable by every account
on the host. `chgrp` needs root, which turns a one-command setup step into a sudo step.
A file's owner may set an ACL on it unprivileged, which is tight and needs no escalation.
`setfacl` is checked for and its absence is a hard failure naming the `chgrp` alternative,
rather than a silent fallback to something weaker.

**The grant is verified, not assumed.** A filesystem mounted without ACL support accepts
`setfacl` and silently does nothing, which would have produced a script that printed
success and a gateway that still crash-looped. `gen-certs.sh` reads the ACL back and fails
if it did not stick.

**Traverse was a second, separate bug inside the fix.** The first attempt granted read on
the key and was still refused, because `mkdir -p` honours the caller's umask: under
`umask 077` the certs directory is `0700` and uid 10001 cannot traverse into it, producing
a byte-identical error message. Granting `+x` to that one uid keeps the directory shut to
everyone else, which `chmod 755` would not.

**`make dev-doctor` now checks it, because the generation-time fix cannot reach existing
installs.** `gen-certs.sh` refuses to run over existing certificates, so every machine set
up before this change keeps the broken permissions forever. The preflight reports the
unreadable key by name with the exact command to fix it, which is the whole point of a
preflight: a crash loop reading `permission denied` on a `.key` file looks like a TLS
configuration problem and is not one.

**Only `gateway.key` is granted.** `ca.key` is never read by any container. `hermes.key` is
read by a container that currently runs as root, so it needs nothing today — that is a
dependency on Hermes staying root, noted in the script where the next person will see it.

**Reopen if:** the gateway stops running as a fixed uid, Hermes stops running as root, or
the certs move out of a bind mount into a secrets mechanism that does its own mapping.

---

## 2026-10-24 — How our CA reaches Hermes, and why it is a bundle

Bringing the stack up for real produced four bugs (`docs/bugs.md` B10) and one open
blocker (B8). Three of the four were the same mistake: a fact about a container's uid
written in one file and contradicted in another. The decisions worth not re-litigating:

**`model` is the dict form, and the `provider:` key is load-bearing.** Hermes' first-run
gate never consults `providers:`. It checks a fixed set of provider env var names —
`PROXY_API_KEY` is not among them — then `auth.json`, then `model.provider` /
`model.base_url` / `model.api_key`. A scalar `model:` misses every one, and Hermes exits 1
claiming no providers exist while the providers block below loads perfectly. Naming the
provider under `model:` both satisfies the gate and routes explicitly through
`providers.chhaya` rather than letting Hermes infer an endpoint.

**The rendered config is 0644 and read-only.** Hermes runs as uid 10000, so 0600 made it
unreadable — and Hermes then runs on *defaults silently*, which reverts sampling,
elicitation, the toolsets allowlist and `skills.write_approval`. Readable rather than
ACL'd because the file carries no secret: `key_env: PROXY_API_KEY` is a variable name and
the value stays in `.env.hermes`.

Read-write was tried, so that Hermes could complete its config migration, and reverted.
s6's `fix-attrs` runs as root and chowns everything under `HERMES_HOME`, which a bind mount
propagates to the **host** file: the rendered config came back `10000:10000 0640`, the
operator could not read it, and `make dev-up` could not re-render. The migration failure is
a warning and every setting still applies. Read-only keeps `hermes/config.yaml` the single
source of truth, which is the property that matters.

**The CA carries `basicConstraints` and `keyUsage`, because Go is lenient and Python is
not.** The CA was generated with neither. Go's `crypto/tls` and curl accept it, which is
why `make dev-status` reported green from the day the certs were created, while Python
refuses it outright with `CA cert does not include key usage extension`. Hermes verifies in
Python. A CA that only some verifiers accept is a CA that works until the one client that
matters tries to use it — and a green status check that exercises only the lenient verifier
is worse than no check, because it is evidence pointing the wrong way.

**Our CA reaches Hermes through `SSL_CERT_FILE`, not through `ssl_verify`.** The obvious
spelling, `ssl_verify: /certs/ca.crt` on the MCP server block, cannot work: Hermes passes
that value to httpx as `verify=`, and httpx 0.28 rejects `verify=<str>` combined with a
client certificate — *"Build an ssl.SSLContext and pass it as verify=<ctx>"* — which a YAML
config cannot express. `SSL_CERT_FILE` is honoured by httpx's default context and needs no
fork.

**It points at a bundle, never at `ca.crt` alone.** `SSL_CERT_FILE` *replaces* the trust
store rather than adding to it. Pointed at our CA, Hermes trusts `chhaya-ca` and nothing
else, and the Telegram connection dies with `CERTIFICATE_VERIFY_FAILED`. Measured: certifi
alone 119 certificates, bundle 120; with the bundle, Telegram, the Anthropic API and
`gateway:8443` all verify. The bundle is built from the **image's own** certifi roots, not
the host's, so the trust set is exactly what the pinned image ships, and it is rebuilt on
every `up` so a pin bump cannot leave a stale one behind.

**`make dev-doctor` checks that each container can read its own key.** `gen-certs.sh`
grants this at generation time but refuses to re-run over existing certificates, so it
cannot reach an install made before the grants existed. The preflight can. This matters
most for `hermes.key`, where the failure names nothing: the MCP handshake simply never
completes, Hermes misses its 60 s startup deadline, and the container crash-loops looking
like a network fault.

**Reopen if:** either container stops running as a fixed uid, the Hermes pin moves (recheck
uid 10000, the certifi path and the httpx version), or the certs move to a secrets
mechanism that does its own uid mapping.

---

## 2026-10-30 — The gateway writes Personal memory, but only on a human tap

**The old promise was enforced by the filesystem.** `scripts/dev.sh` granted the gateway
uid write on `memory/.proposals/` and nothing else, so `memory.propose_update` could stage
a proposal and could not edit a document even if its code tried. Nothing read the
proposals: there was no way to approve one, and the file was `0600` owned by uid 10001, so
the operator could not even read it on the host. "I do not like okra" produced a proposal id
and nothing else.

**The new promise: the gateway edits a Personal memory document only on a human tap on the
approver bot, never from a tool call.** `memory.propose_update` takes one literal line
(`entry`, e.g. `No okra.`), stages it, and posts a card to the approver bot. A tap on
approve appends `- <entry> <!-- proposal <id>, approved <date> -->` to the document. Hermes
is out of the loop between the proposal and the write, so the tap is the gate, not the
agent and not a file permission. `memory.Store.ApplyApproved` has one caller, the tap
handler in `internal/approval`, and no MCP tool reaches it. `dev.sh` now grants the gateway
`rwX` on all of `memory/` except `memory/.git`, with default ACLs on directories for both
the gateway uid and the operator. That keeps a document the gateway creates (`0664`)
writable by the operator. It does not fully cover the other direction: a document the
operator creates `0644` gets ACL mask `r--`, which cuts the gateway's entry to read, so the
gateway cannot append to it until the next `make dev-up` re-runs `setfacl`. A tap on it
fails closed with `⚠️ not written` and keeps its buttons; tap again after `up`.

**The tap still asks `policy.Decide`.** The handler builds a synthetic request — tool
`memory.apply`, tier 0, kind write — and refuses on `Deny`, so the kill switch and the rate
limit hold against a tap exactly as against a tool call: a tap does not outrank `/stop`.
`NeedApproval`, which taint produces, is treated as satisfied, because the tap is the human
approval it asks for. Every tap is audited as actor `approver`, tool `memory.apply`, with
the proposal id and path but never the entry.

**The intent is recorded before the write, and a broken ledger stops the taps.** Before
`ApplyApproved`, the handler appends a record with decision `allow`, rule
`memory.tap_approved`, answer `approved` and status `pending`; after the write, a second
record carries `ok` or the error. If the intent record cannot be appended, nothing is
written: the card goes to `⚠️ not written` with its buttons, and the proposal stays
pending. Any failed approver audit append also sets a latch on the bot, and while it is set
both approve and reject are refused the same way, because a reject moves a file too. Only
a restart clears it — the approver's counterpart to the gateway's `audit.unwritable`
latch. A crash between the two records leaves an intent with no outcome, which says "look
at the document", where writing first would have left a line with no record at all.

**Reject refuses a proposal whose line is already written.** A crash between the append
and the move leaves the line in the document and the proposal pending. Reject reads the
document and, if it holds `<!-- proposal <id>,`, refuses with `⚠️ not written: already
written; approve to finish filing it`, keeping the buttons. Approve does not write the line
twice, and files the proposal under `applied/`.

**What was approved is the card, not the file.** The handler reloads the proposal and
refuses the write if its path or entry differs from what the card showed. Otherwise a
proposal edited on disk after the card went out would be written under an approval nobody
gave to those words.

**A failed write keeps the card's buttons.** A refusal by policy answers the tap and leaves
the card as it was; any other failure edits the card to `⚠️ not written: <reason>` and puts
the same buttons back. The proposal is still pending, so once the cause is fixed the same
card is tapped again rather than the agent being asked to re-propose.

**Append with `O_APPEND`, never write-a-temp-file-and-rename.** A rename replaces the inode,
so the document would come back owned by uid 10001 with the directory's default ACL in place
of the operator's file and its own grants. It would also discard any write made between the read
and the rename. `O_APPEND` keeps the inode, owner and ACL, and each line lands whole.

**A document the gateway creates is `0664`.** With an ACL on the directory, a file's group
bits become the ACL mask. A `0644` create sets the mask to `r--`, which cuts the operator's
default `rwX` entry down to read, and the operator could no longer edit a document the
gateway had created. `0664` keeps the mask at `rw-`.

**Proposals in the old `diff` format are shelved, not converted.** On start the bot moves
any proposal with no `entry` to `.proposals/legacy/` and logs it. A diff was free text with
no single line to append, and turning it into one would be the gateway writing words the
user never saw on a card.

**Reopen if:** anything inside the gateway other than the tap handler writes to `memory/`,
or any MCP tool gains a path to `ApplyApproved`. Either one turns the grant on `memory/` into
a write the agent can cause without a human.

---

## 2026-11-06 — Hermes' home is a host bind mount, not a Docker volume

**Hermes' state must outlive Docker.** `/data` held `memories/`, authored `skills/`,
`state.db` (the source for trace ingestion) and `.env`, in the named volume `hermes-data`.
`docker compose down -v`, `volume prune` or losing the machine took all of it, and
`design.md` promised a mounted volume with snapshots. The Hermes Docker docs use a bind mount
(`~/.hermes:/opt/data`); a named volume appears there only as a workaround for `state.db` on
virtiofs/9p, which does not apply on Linux ext4.

**Path:** `$CHHAYA_HERMES_HOME`, default `~/.chhaya/hermes`, created `0700` by
`scripts/dev.sh`. `config.yaml` and `SOUL.md` are pre-created there so the Docker daemon
does not make root-owned mountpoints for the read-only file mounts.

**`HERMES_UID`/`HERMES_GID` are the operator's.** The image's s6 stage remaps its `hermes`
user to them. Verified on a scratch directory: every file, `.env` included, came back owned
by the operator and nothing by 10000. This also removes the old `fix-attrs` hazard of
chowning host files to 10000. `readable_by` and `traversable_by` now count owner bits, since
`hermes.key` is owned by the uid Hermes runs as.

**Not done:** `gateway-data` (the audit log, kill-switch state) is still a named volume and
has the same durability gap. There is no backup job yet; the directory is now an ordinary
host path for one. Use `sqlite3 state.db ".backup"` for a consistent database copy.
The old `deploy_hermes-data` volume was copied, not deleted.

**Reopen if:** the Hermes pin moves (recheck that `HERMES_UID` is still honoured), or the
deployment moves to AWS, where this becomes an EBS mount.

---

## 2026-11-07 — The host is trusted; the gateway's data is a bind mount too

**Assumption, stated so it stops being re-derived:** the machine running the stack is
secure. We do not defend the audit log or state files against the operator or other host
users. The boundary that matters is agent versus gateway, and both stay as they were (mTLS,
separate bot tokens, separate containers). Hardening against the host (append-only
attributes, separate uids, ACL gymnastics) is out of scope until the deployment is not a
single-user machine of ours.

**`gateway-data` was a named volume** holding `audit.jsonl`, `state.json` and `policy.hash`,
with the repo's `memory/` bind-mounted on top at `/data/memory` — a volume and a bind mount
nested, two ext4 mounts at `/data` and `/data/memory`. `docker volume prune` would have
deleted the audit log, and the gateway reads a missing log as a legitimate fresh start, so
the evidence would vanish without an error.

**Now:** `$CHHAYA_GATEWAY_HOME` (default `~/.chhaya/gateway`, 0700) is bind-mounted at
`/data`, and the gateway container runs as the operator's uid (`user:` in compose). Files are
the operator's own, `make dev-verify` and direct reads from the host work, and no ACLs are
needed for this directory. `dev.sh` pre-creates `memory/` inside it for the nested mount.
The old `deploy_gateway-data` volume was copied (audit chain verified intact: 27 records)
and not deleted.

**Left as is:** the `setfacl` grants for uid 10001 on `memory/` and `deploy/certs` are now
redundant, because the gateway no longer runs as 10001. They are harmless and removing them is
a separate change. The `USER chhaya` in the Dockerfile is overridden by compose.

**Reopen if:** the host stops being trusted (shared machine, cloud instance with other
tenants), or the audit log needs tamper resistance against the operator.

---

## 2026-11-07 — The agent's file-write root is `/data/workspace`

**The image default does not fit our layout.** The Hermes image sets
`HERMES_WRITE_SAFE_ROOT=/opt/data`, and the entry on `decisions.md` line 199 ("we do not
need to set it ourselves") was verified with a home of `/opt/data`. Ours is `/data`
(`HERMES_HOME`), so the root pointed at a directory Hermes does not use. Found in the logs:
`Write denied: '/data/digest_feedback/AGENTS.md' is outside HERMES_WRITE_SAFE_ROOT
(/opt/data)`. The recurring "Daily ML/AI Advancements Digest" job the user asked for had
`workdir: /opt/data/digest_feedback`, which is allowed but lives in the container's own
filesystem and is lost when the container is recreated.

**Now:** `HERMES_WRITE_SAFE_ROOT=/data/workspace`, a directory `dev.sh` creates in the
bind-mounted home. Probed against the pinned image with `agent.file_safety`: writes under
`workspace/` are allowed; `skills/`, `.env`, `memories/`, `config.yaml`, `SOUL.md`, `cron/`
and a `workspace/../skills` traversal are denied. The job's `workdir` is now
`/data/workspace/digest_feedback`, and `SOUL.md` tells the agent where its working files go.

**Not `/data`, on purpose.** That would put `skills/` in reach of `write_file`. A skill can
declare `required_environment_variables`, and `skills.write_approval` is what closes that
path — but it gates the skill tools, not a plain file write. Not verified whether a file
write there would bypass it; the narrow root makes the question moot.

**Not verified:** whether the `terminal` tool honours the safe root. A `terminal` call
appears in the agent log, and its sandbox has its own rules.

**Reopen if:** the Hermes pin moves (recheck the default and `file_safety`), or a task needs
to write somewhere outside `workspace/`.

---

## 2026-11-08 — `memory.list` added during the feature freeze

**Why it is an exception.** Sessions in `state.db` showed the agent "forgetting": asked for
coffee near home, it made no memory call and asked for the address, which was saved in
`location`; told to check, it guessed `location`, `addresses`, `home`, `profile`. The gateway
had only `memory.get` (exact path) — `memory.search` was in `design.md` and never built. The
user asked for the fix before the deployment and eval work, because the live setup is still
the one gathering the sessions we will learn from.

**What it is.** `memory.list`: tier 0, read, trusted, no arguments. Returns each document's
path (exactly what `memory.get` takes), title and entry count — never content. Capped at 500
with a `truncated` flag. It skips dot-prefixed files and directories (`.git`, `.proposals`:
staged proposals are not memory yet), symlinks, and non-Markdown files; an unreadable root is
an error, not an empty list. Tests prove each of those stays out of the result.

**Not just a tool.** The agent also did not look before asking, though `SOUL.md` said to call
`memory_get` first. `SOUL.md` now says to list and read memory before asking the user for a
fact about themselves. Whether that holds is exactly what the recall eval should measure.

**Reopen if:** memory grows past what a listing can carry (then `memory.search`), or the agent
still asks before looking after this change.

---

## 2026-11-08 — Personal memory is data, outside the public repo

**The repo is public.** Personal memory holds a home and office address and a person's
tastes, and it was tracked in git because `design.md` called it "the asset": versioned,
revertable, with provenance. That reasoning was about having history, not about having a
remote. Checked before moving: only the generic seed `taste/food.md` had ever been pushed;
`interests.md` and `location.md` were never tracked, so there is no history to rewrite. The
risk was live, though: `food.md` was tracked and modified, so a `git commit -a` would have
published it.

**Now:** memory lives at `$CHHAYA_GATEWAY_HOME/memory` (default `~/.chhaya/gateway/memory`),
inside the gateway's one bind mount — the nested `../memory` mount is gone. It is its own
local git repository with no remote; `dev.sh up` commits what changed
(`snapshot_memory`), `.proposals/` is ignored. Off-box safety is the backup job's concern.
`memory/` is in `.gitignore` so a stray copy cannot be committed; `memory.example/` is a
generic seed (the original seed text from history).

**Removed:** `prepare_memory`, the `setfacl` grants for uid 10001 on `memory/`, and the
matching `dev-doctor` checks. The gateway runs as the operator (2026-11-07), so none of it
did anything. This supersedes the `memory/` ACL notes in the 2026-10-30 entry and the
"Left as is" paragraph in 2026-11-07.

**Verified:** the moved tree was byte-identical to the original (`diff -r`), the audit chain
still verifies (46 records), and a live mTLS call to the running gateway returned
`memory.list` and `memory.get location` from the new path. **Not verified:** an approval tap
writing to the new location; that needs Telegram.

**Costs:** dev and prod no longer share memory (intended); the design's replay-harness
pin of "git SHA of Personal memory" now means a commit in the local repo.

**Reopen if:** memory needs more than one writer or a second machine, which is when a
private remote becomes worth its upkeep.

---

## 2026-11-08 — Deployment plan: one EC2 box in Mumbai, Terraform, separate bots per environment

Recorded in full in `docs/deployment.md`. The choices, so they are not re-litigated:

- **Production is one EC2 instance in ap-south-1**, built with Terraform committed under
  `infra/terraform/`. The desktop becomes development only.
- **No inbound traffic and no SSH.** The design needs none (Telegram polling, gateway on
  localhost); the only reason to expose the box was shell access, which SSM Session Manager
  covers without open ports or key pairs.
- **Two bot pairs, one per environment.** Two pollers on one token get Telegram 409s, drop
  messages and can misroute an approval tap. Production keeps the existing `chhaya_bot` and
  `chhaya_approver_bot`; dev gets new `chhaya_dev_bot` and `chhaya_dev_approver_bot`. A
  preflight `getMe` check enforces the naming against `CHHAYA_ENV`, so the production token
  cannot start on the desktop. It fails closed if Telegram is unreachable.
- **Production takes all current data**, as a copy. The desktop keeps its own, with the
  digest cron job disabled so only production sends it.
- **The handover is the first restore**, via a private S3 bucket, so the backup and restore
  script is written and tested for real before the eval work reuses it.

**Reopen if:** the pinned Hermes image has no arm64 build and cost matters, the box needs a
second user or inbound access, or the bot naming convention stops being enforceable.

## 2026-11-09: Bot-environment guard (`CHHAYA_ENV` + `getMe`)

**Decision.** Both env files declare `CHHAYA_ENV=dev|prod` (exact, no default, must agree). Preflight (`make dev-doctor`, `dev-up`) runs `chhayactl botcheck`, which calls Telegram `getMe` on each token and refuses unless: dev bots' usernames contain `_dev_` and prod ones don't; the approver's username contains `approver` and Hermes' doesn't; the two tokens are different bots. Unreachable Telegram, a missing or unknown `CHHAYA_ENV`, or Go missing on the host are refusals. Logic lives in `internal/botguard` (tests cover each refusal); errors and output never include a token.

**Why.** One bot token, one poller: two machines on the same bot drop messages (409) and can misroute an approval tap. Dev and prod have separate bot pairs (`docs/deployment.md`), and the only safe way to keep it that way is to check, not to remember.

**Consequences.** The check needs Go on the host (the box's `user_data` must install it, or the check moves into a container). It checks at start only; it cannot stop someone editing a token after `up`. The gateway does not re-check its own token at runtime yet. Existing env files must gain `CHHAYA_ENV` (the current desktop setup uses the prod bots, so `prod` until step 2 of the deployment plan).

## 2026-11-09: Production is live on EC2; certificate ACLs removed

**Decision.** Production runs on the Mumbai box (`docs/deployment.md`), started 2026-11-09 and smoke-tested over Telegram. The desktop is dev, on the dev bots. `deploy/gen-certs.sh` no longer calls `setfacl` and `scripts/dev.sh` preflight checks key readability against the operator's uid instead of 10001 and 10000.

**Why.** Both containers run as the operator's uid since the bind-mount move, so a `0600` key the operator owns is already readable by the process that needs it. The ACL grants were left over from the old uids and made `make dev-certs` fail on the fresh box (`setfacl: command not found`).

**Consequences.** `chhayactl backup` is the one way data moves between machines (see the next entry). Open follow-ups are listed at the end of `docs/deployment.md`.

## 2026-11-09: Backup is Go, in `chhayactl`, run from the host with the stack stopped

**Decision.** `chhayactl backup create|verify|restore|push|pull|status`, implemented in `internal/backup`. A nightly job (cron on the box, 02:00 IST) runs `scripts/dev.sh backup`: stop both containers for a few seconds, take a cold copy, restart, upload to `s3://<bucket>/backups/`. S3 keeps 30 days (lifecycle rule); the box keeps three locally. Replaces the shell script.

**Why not inside `chhayad`.** The daemon only sees the gateway's data, not Hermes' home; backing up both would mean mounting agent-writable files into the security boundary. It would also pull an AWS SDK and a SQLite driver into the most trusted process and give it write credentials, and restore must work when nothing is running. A backup is not an agent action, so the kill switch and policy tiers should not govern it.

**Why stop the stack.** A cold copy is exactly consistent, so the code needs no SQLite library: databases are copied as files and checked with the `sqlite3` CLI (`PRAGMA integrity_check`; fails closed if the CLI is missing). Nobody talks to the agent at 02:00. A real maintenance mode (see `docs/backlog.md`) replaces the stop later.

**Format and checks.** One tarball with `MANIFEST.json` as its last entry (every file's size, mode, SHA-256) plus a `.sha256` sidecar. Restore needs both: the sidecar catches damage in transit, the manifest catches an archive rebuilt with a fixed-up sidecar. Restore never overwrites, extracts to a temp directory, then moves into place; refuses symlinks, special files, absolute or `..` paths and anything outside `hermes/` and `gateway/`. Lock files and sockets are excluded. Create refuses a failing database or a broken audit chain. Each safety check has a test that fails when the check is removed (mutation-checked).

**Consequences.** Upload and download shell out to the `aws` CLI so the instance role is used and no AWS SDK enters our code. The box stops for a few seconds nightly; a missed run shows as a stale-backup warning in `make dev-status` on prod. Remote triggering (EventBridge to SSM Run Command) is the next step and changes nothing in the tool.

## 2026-11-09: The box's requirements live in `scripts/box-setup.sh`, not in `user_data`

**Decision.** `scripts/box-setup.sh` installs everything the box needs and is idempotent; `infra/terraform/user_data.sh` only installs git, clones the repo and runs it. `--check` changes nothing and lists what is missing; `--cron` also installs `deploy/crontab.box`.

**Why.** Terraform ignores `user_data` edits on purpose (`ignore_changes`), because replacing the instance would delete its data. That left the live box and its bootstrap free to drift: `sqlite3` was added to the script but had to be installed by hand. A script in the repo can be re-run on the live box after a `git pull`.

**Consequences.** First boot now runs code from `main` of the public repo, so a bad push to `main` affects a new box. The backup cron is opt-in because a fresh box has no data yet.

## 2026-11-09: Evals are driven by error analysis, not a predefined label list

**Decision.** Observability and evals follow `docs/observability-and-evals.md`: read real traces, note pass or fail and the first failure in free text, group notes into failure modes and count them, then write replayable cases for the modes that persist. Checks are code over the trace wherever the failure is visible in tool calls; an LLM judge is used only for what code cannot see, and only after it agrees with my labels on held-back traces. A change ships when its cases pass more often than a baseline run on the same cases with no regression. Traces are read with `chhayactl trace` (read-only, via the `sqlite3` CLI) on a restored copy of prod; real notes and cases live in `~/.chhaya/evals`, never in this repo.

**Supersedes.** The label schema in `docs/design.md` (nine failure modes chosen in advance, multi-select) and Langfuse as the first UI. The tiers, the learning metrics and the monthly ablation in `docs/design.md` still stand.

**Why.** A taxonomy written before any data names the failures we imagined, not the ones we have; the first real failure (the agent "forgetting", `docs/learnings.md`) was two modes, neither of which the list separated. Generic judged scores were not calibrated against anything. Langfuse is a four-service stack for one user and about 150 traces a month; a CLI over `state.db` answers the same questions now.

**Reopen if:** labeling in the CLI is slow enough that traces go unread, trace volume outgrows reading every one, or the audit-to-session join by time turns out ambiguous.

## 2026-11-10: S3 is the system of record for traces and annotations; the page is local

**Decision.** `chhayactl trace export` runs on the box after the nightly backup and writes one trace object per session to `s3://<bucket>/evals/days/YYYY-MM-DD/trace-<id>.json`. `chhayactl trace serve` is a local page on `127.0.0.1` that reads those traces from S3, autosaves annotations locally and pushes `open-code-<id>.json` beside each trace with an Export button. Details in `docs/observability-and-evals.md`.

**Why S3 and not a hosted page.** Traces hold family, health and location, and the bucket is private. S3 website hosting needs a public bucket or CloudFront with its own login, and a browser cannot write to a private bucket without credentials. A local page adds no way in, and S3 still gives durability, versioning and one place to restore from.

**Consequences.** Labeling is desktop-only for now; a presigned-URL variant for a phone can implement the same three-call data interface. `evals/` needs its own lifecycle rule (no expiry). The box role's `DeleteObject` on `bucket/*` is broader than needed and is the owner's to narrow.

**Reopen if:** labeling on a phone matters enough to justify presigned URLs, or the unsynced-annotation window loses a note.

## 2026-11-10: S3 expiry rules are scoped by prefix; `evals/` expires nothing

**Decision.** The bucket's lifecycle rules in `infra/terraform/s3.tf` name their prefixes: `backups/` (current objects 30 days, then noncurrent 30), and noncurrent-version expiry of 30 days on `handover/`, `terraform/` and `tools/`. A bucket-wide rule remains only for aborting incomplete uploads. `evals/` is in no expiry rule.

**Why.** The old `expire-old-versions` rule had an empty filter, which is the whole bucket. An S3 lifecycle filter can include a prefix but cannot exclude one, so an `evals/` rule could not override it; annotations are overwritten in place, and their old versions would have been deleted after 30 days. Annotations and eval cases are the owner's labelled data and are tiny.

**Consequences.** A new top-level prefix gets no expiry until it is added to `expiring_prefixes`; it fails safe, but it can grow. The Terraform state file under `terraform/` keeps the same 30-day rule it had.

**Reopen if:** `evals/` grows large enough to cost money (traces are the part that would), at which point trace objects could move under their own prefix with an expiry, leaving annotations alone.

## 2026-11-11: traces record what the model was sent, from Hermes' own request dumps

**Decision.** `trace export` reads `sessions/request_dump_*.json` and adds the system prompt, tool schemas and per-call messages to the trace (`trace.llm`, schema 2). Hermes writes those dumps on every call when `HERMES_DUMP_REQUESTS=1`. We set it in the per-machine `deploy/.env.hermes` on the desktop; the box stays off until it is turned on deliberately.

**Why.** The first annotated traces showed the conversation but not the system prompt, the tool list or what each request carried, and error analysis needs what the model saw. `state.db` does not store it. Hermes already has the capture, so a proxy (LiteLLM) or a tracing service (Langfuse) would add a component, and a place memory text flows through, for nothing we lack.

**Consequences.** Trace objects grow (the prompt and tools once per trace, plus each call's new messages). Memory text from the system prompt now lives in traces in the private bucket. Only sessions run with the flag have the data.

**Reopen if:** the dump format changes on a Hermes upgrade (the reader names the keys it uses and reports unreadable files in `problems`), or we need the data for sessions the dumps did not cover.

## 2026-11-12: code checks over traces have three levels, and run on the traces the bucket holds

**Decision.** `chhayactl trace check` runs a fixed set of deterministic checks (`internal/check`) over exported traces. Each check has a level that belongs to the check: `fail` (an invariant; exits non-zero), `flag` (look at it) or `info` (inventory). Behavioural checks apply to Telegram sessions only, because cron digests behave differently. A trace that cannot be read is reported and makes the run fail. With `-s3` the bucket's copy is read, never a local one.

**Why.** Error analysis says to start with checks you can state exactly, and to keep fuzzy signals out of the pass/fail path until your own labels say they matter. Writing the first set against the 19 existing traces showed where a naive version lies: a text match for `"error"` fires on `"error": null`, and injected `[Cron delivery]` messages stored as user messages look like unanswered questions. Both are handled by parsing and by a defined "human message".

**Consequences.** `ask.before_memory` is a flag, not a failure, until annotations show it agrees with what the owner calls wrong. `tools.surface` is inventory until the owner sets an allow-list of Hermes built-in tools per source. Thresholds for tool-call counts and latency are not set until there is a baseline.

**Reopen if:** a check disagrees with the owner's labels often enough to be noise (fix or drop it), or a failure mode appears in annotation that no check can state (that is the case for a calibrated judge).

## 2026-11-12: a trace is one conversation, split at 60 minutes of silence

**Decision.** `trace export` splits a Hermes session into conversations wherever the time between two consecutive messages is longer than 60 minutes (`-gap`, 0 disables). One trace is one conversation and stays multi-turn. The first keeps the session id and later ones are `<id>.2`, `<id>.3`. Per-turn traces were considered and rejected by the owner: follow-ups need the turns before them.

**Why.** Hermes keeps a Telegram chat as one session until `/new`, and core no longer resets on a timer, so sessions hold days of unrelated conversations (one pinned-image session ran 2.4 days; the first four real sessions split into 11 conversations). Annotation needs a unit a person can judge. The split is on our side, so it works whatever Hermes does and stays valid after a reset policy is added.

**Consequences.** Annotations attach to a conversation id. A later conversation's first model request still carries the whole earlier session (marked `carried_messages`), which shows how much unrelated history the model saw. `session.not_reset` is a check that reads the same gaps to tell whether an idle reset works. A tool that runs longer than the gap would split a call from its result; none has been seen.

**Reopen if:** 60 minutes cuts real conversations in two (annotation would show a trace that starts mid-thought), or sessions are reset by policy often enough that the split rarely fires.

## 2026-11-15: every session's cost is computed by us and sent to Telegram; dev runs the cheapest model

**Decision.** `chhayactl cost report` prices each Hermes session from the token counts in `state.db` using `config/pricing.yaml`, and with `-send` tells the owner on Telegram, from the approver bot, once per finished session (quiet 35 minutes, or ended by Hermes; grown sessions are re-announced as the difference). A cron line in `deploy/crontab.box` runs it every 10 minutes and `box-setup.sh --cron` installs it, so a rebuilt box gets it. Dev (`CHHAYA_ENV=dev`) runs `claude-haiku-5-5`, and so does prod for now (the owner's call the same day: the digest skill is explicit enough that Haiku does it well). Moving a task back to Sonnet is the later per-task routing feature (backlog). `make dev-doctor` warns when dev is on anything else.

**Why.** Hermes records tokens but not cost against a direct provider (`cost_status` is "unknown", `estimated_cost_usd` 0), so there was no number to look at. The first measurement: $30 over 15 desktop sessions in nine days, nine of the ten dearest being cron sessions at $2 to $4 each, each call carrying about 85k prompt tokens with no caching (`cache_read_tokens` is 0 on every session). Haiku 5.5 is $0.10/$0.50 per million tokens against Sonnet 5's $2/$10.

**Consequences.** The price table is ours and must be kept current; a model with no entry is reported as "no price", never as $0, and a cache price that is not published is not guessed (the session is marked partial). Haiku's long-prompt tier ($0.50/$2.50 over 100k) is applied from the session's average prompt per call, so it is an estimate and says so. The message goes out from the gateway side (approver bot), not from Hermes, so the agent cannot suppress or edit its own bill. Sessions carry the model of their first call, so a session that switched models is priced at that one.

**Reopen if:** Hermes starts writing a trustworthy `actual_cost_usd` (then read it and drop the table), or the provider moves behind a proxy that reports usage per request (then price per request and drop the tier estimate).

## 2026-11-16: prod chat runs on Sonnet, the digest cron is pinned to Haiku

**Decision.** Prod's `CHHAYA_HERMES_MODEL` is `claude-sonnet-5-5`, so Telegram conversations use it. Cron jobs named in `hermes/cron-models.tsv` carry their own model through Hermes' per-job `model`/`provider` fields; the digest is pinned to `claude-haiku-5-5`. `scripts/dev.sh cron-models` applies the file (idempotent, `--check` reports drift) and writes only through `hermes cron edit`. Dev stays on Haiku everywhere. Supersedes the "prod on Haiku for now" half of 2026-11-15.

**Why.** Trace 2026-11-16 (podcast request): Haiku read "this is also done :)" as approval and ended the turn, and told the owner a fact about a past session it could not see. The digest, by contrast, runs a fixed skill and Haiku does it well, and it is nine of the ten dearest sessions. So the split follows the work: judgement in chat, a scripted pipeline in cron.

**Consequences.** The pin lives in Hermes' job file, not in this repo, so the repo holds the intent (the tsv) and the script re-applies it; run it after any job is recreated. A session keeps the model of its first call, so a conversation open at the restart stays on Haiku until `/new` or the idle reset. Not yet checked: whether Hermes' auxiliary calls (titles, compression) follow the main model and so now bill at Sonnet rates.

**Reopen if:** the cost report shows chat on Sonnet costing more than the owner will pay for, or a second cron job is added (then decide whether routing belongs per skill).

## 2026-11-16: the deterministic checks run on every session, nightly; three new ones from the podcast trace

**Decision.** `chhayactl trace check` runs from `deploy/crontab.box` at 20:50 UTC, after the export, over the IST day that ended and the new one, logging to `~/.chhaya/backups/check.log`. `box-setup.sh --cron` requires the line. Three checks were added: `reply.owner_correction`, `recommend.unsearched`, `routing.chat_model`.

**Why.** Until today zero evals ran on production sessions. The eleven existing checks, run by hand on 2026-11-16, found nothing wrong with the one session that went wrong (three recommendations the owner had already consumed, a reply "done :)" read as approval): they cover integrity and tool errors, not whether the answer was right. The new checks fire on exactly that session and on none of the traces from Nov 11 to 15.

**Consequences.** All three are heuristics on wording and are `flag`, never `fail`. The correction cues are a list of phrases and will miss other ways of saying it; each miss found in a trace is a phrase to add, with a test. Results are in a log on the box, not yet in Telegram or S3.

**Reopen if:** `owner_correction` fires on traces the owner calls fine (then it is noise: tighten the cues), or the log goes unread for a week (then push it to Telegram).

## 2026-11-16: memory is a module with its own tests; the nightly agent may reorganize without a tap, but adding facts still needs one

**Decision.** The dreaming pipeline (`docs/dreaming.md`) works on a branch of the memory repo and lands a change only when code-run gates pass: fact survival, the probe suite, the owner's golden set, and shape. By risk: reorganizing and merging exact duplicates are automatic once the gates pass (merged by the gateway, not the agent, then summarized to the owner); adding a fact needs a tap, batched into one card; rewording, superseding or deleting a fact needs a tap always. A nightly canary replays the tests against live memory and the gateway reverts the last commit if something that passed now fails. The tests are the owner's and the agent has no write path to them. Rollback is the gateway's call, never the agent's.

**Why.** The owner's proposal: automatic, versioned, tested, rolled back at runtime. Taken whole it removes the human from the one change where a mistake is expensive (a wrong inference stored as truth), and the runtime signal is too thin to drive a rollback (one user, a handful of sessions a day, checks that fire a few times a week). The lossless tier can be proved safe by a deterministic gate, so it needs no tap; new facts cannot, so they keep one until the extraction evals say otherwise.

**Consequences.** CLAUDE.md's non-negotiable "never edit Personal memory directly; only `memory.propose_update`" gains one exception, the gateway merging a gated lossless branch. It is amended when that lands (build step 5), not now. New gateway work: a `dreamer` client with its own policy and blocked-case tests. Auto-applying additions is a separate later decision and would need a veto window. Building the tests, gates and canary (steps 1 to 3) fits the 2026-11-08 freeze; the rest needs the owner to lift it.

**Reopen if:** the approval rate on batched fact proposals is high and steady (then auto-apply with a veto window), the tests prove too weak to catch a bad reorganization (then the lossless tier needs a tap too), or the owner's own signals start to carry enough data to drive a rollback.

## 2026-11-16: the memory tests live in `<memory root>/.tests/`, and the memory tools cannot open dot-directories

**Decision.** Probes (`probes.json`) and the golden set (`golden.json`) live in the memory repo under `.tests/`: versioned and backed up with the memory, never in the public repo (a test fails if a tracked file looks like test data). `internal/memtest` checks them without a model: a fact line with no proposal id, an id on two lines, a probe about a fact that is not in memory, a malformed probe or golden question all fail; a fact with no probe is a warning. `chhayactl memtest check` runs it. `memory.Resolve` now refuses any path component starting with ".", so `memory.get` cannot read `.tests`, `.proposals` or `.git`.

**Why.** An agent that can read its own answers cannot be tested by them. Before this the only thing keeping `.tests/x` unreadable would have been the `.md` suffix `Resolve` appends: a file named `.tests/x.md` was readable (test `TestResolveRefusesDotDirectories` failed on the old code). Golden questions can also forbid phrases (`expect_none`), because the podcast failure is a recommendation that repeats what the owner already heard, not a missing answer.

**Consequences.** First run against prod's memory: 20 facts with ids, 3 lines in `taste/food.md` with none (seed content from the M0b smoke test), so the check fails until the owner decides what happens to them. Probes were drafted by Sonnet, the same family as the agent they test; `memory-selftest.md` R1 wants a different model, which remains open. Not built: the runner that answers the probes with a weak model (S2), and the canary that runs the check nightly.

**Reopen if:** a legitimate memory path starts with a dot, or the owner wants the tests kept outside the memory repo.

## 2026-11-17: every approved memory write is its own git commit

**Decision.** `ApplyApproved` appends the line, then commits that one document to the memory repo (`memory: add to <path> (proposal <id>)`, author `chhaya-gateway`), then files the proposal as applied. The commit is `memory.GitCommit`, set on the approver's store in `chhayad`; a `Store` with no `Commit` (unit tests) keeps no history. If the commit fails the proposal stays pending, the card says "not written", the audit log records the error, and the owner's second tap retries without writing the line twice. The gateway image now carries `git`. The commit takes only the written document, runs git with no system or global config, no hooks and no signing, and does nothing when there is nothing new to commit.

**Why.** Before this the memory repo held only occasional `snapshot:` commits (four between Nov 8 and Nov 11), so "revert the last write" would have undone whatever had accumulated since. The dreaming design (`docs/dreaming.md`) rolls memory back with `git revert` and attributes a bad answer to a recent commit; both need one commit per write. A failed commit blocks the write's completion rather than being logged and ignored, because history with silent gaps is the failure this exists to remove.

**Consequences.** The first commit after deploy also takes any approved lines that were never committed in the same document. `scripts/dev.sh` snapshots still run and now usually find nothing to commit. Hooks in the memory repo never run. A box whose memory root is not a git repo can no longer complete approvals; that is the fail-closed choice and the error says why. Not covered: edits made by hand (like the 2026-11-17 seed-line removal) are still committed by whoever makes them.

**Reopen if:** commit failures become a nuisance (then retry in the background and alert, but never mark applied without the commit), or memory moves out of git.

## 2026-11-17: the memory runner answers the probes with a weak model through the chat-completions endpoint, in Go

**Decision.** `chhayactl memtest run` gives a small model (default `claude-haiku-5-5`) two tools, `memory_list` and `memory_get`, and asks it every probe and golden question. The tools are the gateway's own code (`memory.List`, `memory.Resolve`) pointed at a temporary copy of the memory that holds no `.tests`, `.proposals` or `.git`. It calls the same OpenAI-compatible endpoint Hermes uses (`CHHAYA_HERMES_API_BASE`, key `PROXY_API_KEY`), not Hermes and not the gateway. What gates (non-zero exit): a probe about a fact whose line never reached the model, and a golden question that fails and is not marked `pending`. A second model call (default Sonnet, not the one tested) judges whether each probe answer matches the expected one; it is reported as WRONG and does not gate. A golden question marked `pending` is known to fail until a fact exists; it is reported, not counted.

**Why.** The Hermes one-shot spike (does `--oneshot` load our MCP gateway?) was never needed: the runner only has to test whether memory's structure and tool interface let a weak model find a fact, and calling the API from Go keeps it independent of the Python container, as `memory-selftest.md` allowed. Deterministic checks gate because they are the same every night; the judge does not, because it is a model nobody has compared with the owner's own reading yet. The first run showed why: Haiku as judge marked "supports Real Madrid" wrong against an expected "Real Madrid", and a correct GEB answer wrong because the expectation listed two books. Sonnet judges now, expectations that list alternatives say "or", and the verdicts stay informational.

**Consequences.** A run costs a few cents (about 90k tokens in for 30 questions on Haiku, plus the judge). The API rejects `temperature` for current models, so runs are not deterministic: the same probe can pass once and miss once; run it several times before trusting a single MISS (a `-k` flag is not built). Three questions in a row that cannot reach the model end a run. The tool descriptions the model sees are not the gateway's exact text (underscores for dots). What it does not measure is the live agent's behaviour: whether it calls memory before asking is the `ask.before_memory` check.

**Reopen if:** a MISS turns out to be noise more often than not (then require 2 of 3), or the judge and the owner agree on a month of verdicts (then let WRONG gate).

## 2026-11-18: a morning report to Telegram, with the detail on a presigned S3 page

**Decision.** `chhayactl report nightly` runs at 07:30 IST (`deploy/crontab.box`). It re-runs the trace checks over yesterday and today, runs the memory tests, reads the canary's `memrun.json` (`memtest run -json`), uploads an HTML page to `s3://BUCKET/evals/reports/DAY.html`, and the approver bot sends one message: a headline (all clear / read the report / FAILED), counts, check names and a presigned link. The message never holds a finding's text; the page does, HTML-escaped, in the private bucket. A part that did not run (no `memrun.json`, one older than 26 hours, memory tests unreadable) is FAILED, not a pass. A clean night still sends one line, so no message at all means the job broke. The judge's WRONG count is shown and never changes the headline.

**Why.** The nightly checks wrote to log files nobody reads; a failure that does not reach the owner is not an eval. 07:30 and not 02:30 because the box signs with an instance role, so the link dies when its credentials rotate (about 6 hours); at 02:30 it would be dead by breakfast.

**Consequences.** `memtest run` writes its result file even when it fails, since a failing night is the one the report most needs. An upload or presign failure is returned but the message still goes, saying the report is missing. Not built: a Telegram button to open an old report (the link is short-lived by design; the page can be re-presigned by running the command again).

**Reopen if:** the 6-hour link is a nuisance (then a small IAM user for presigning, which puts a secret on the box), or the clean-night line becomes noise.

## 2026-11-19: the extractor and its evals come first; the policy is in `Filter`, not the prompt

**Decision.** `internal/dream` holds the extractor (`Extract`: a model reads one Telegram conversation plus the stored facts and suggests facts) and `Filter`, which decides what survives. Filter is code: a candidate must cite owner-typed messages only (`check.IsHuman`: not the assistant, not tool output, not an injected "[System note", not an inactive message), name an existing document, be one valid line (`memory.ValidateEntry`), not repeat a stored or earlier line, and fit the cap (a missing cap is zero; the rest are counted). A reversal of a stored fact must name that fact's id and is reported as a contradiction, never added; a plain add that restates it is dropped. A cron run is not eligible. `chhayactl dream eval` runs 11 synthetic cases shipped in `internal/dream/cases` (recall, silence, no duplicate, contradiction, two injections, cap) and, with `-memory`, private cases from `MEMORY/.tests/dream`, where the real podcast conversation lives. Nothing here proposes or writes: no gateway, cron or approver change yet.

**Why.** docs/dreaming.md says the evals are written before the pipeline. Injection and the cap are the cases a prompt cannot be trusted with, so the model may only suggest and the filter decides; a test feeds a fooled model's output straight to Filter and shows the line is dropped.

**Consequences.** First real run (Sonnet): 9 of 11 passed. The two failures were prompt faults, not model faults: it picked 2 of 5 stated facts (fixed by telling it to return every stated fact), and it filled a contradiction's `entry` with the old line (fixed by saying `entry` is the replacement). After that, 11 of 11 on three runs, and the real conversation passed 3 of 3, proposing the Drig-Drishya Viveka, Vigyan Bhairav and Osho lines and citing the owner's messages. The prompt was tuned on these cases, so they now measure regression, not generalisation: the real conversation and new private cases are the held-out part. The model is not deterministic: one run of the synthetic recall case added "listens to podcasts on the commute" as noise. `-runs N` reports a pass rate per case.

**Reopen if:** noise (a true fact the case did not expect) turns out to be the common failure, which would mean cases should allow extra true facts and count only false ones.

## 2026-11-19: the dreamer is a second gateway client, named by its certificate, with an allowlist in code

**Decision.** The gateway now knows who is calling. `mcpsrv.ActorHandler` takes the actor from the verified client certificate's common name (it discards any `X-Chhaya-Actor` header the client sent, and refuses a request with no verified certificate); `Invoke` passes it to `policy.Decide`, which denies an actor it does not know (`actor.unknown`), before even the unknown-tool rule. The `dreamer` may call exactly `memory.list`, `memory.get` and `memory.propose_update` (`actor.tool_not_allowed` for anything else; the list is code in `internal/policy`, not config). It has its own hourly counters (`CallKey`; Hermes keeps the bare tool name so counts on disk stay valid) and a daily proposal cap, both from `config/policy.yaml` (`dreamer:`). An absent section, or a zero, means the dreamer can do nothing. Only a proposal that was accepted counts against the cap. Taint, which is the browsing session's state, does not turn a dreamer proposal into a second approval; the tap on the card is its approval. `deploy/issue-client-cert.sh NAME` issues a client certificate from the existing CA, and `gen-certs.sh` issues `dreamer` too. Nothing mounts the dreamer's key into a container. The approver bot's own `memory.apply` also goes through `policy.Decide`, so `approver` is a third known actor that may call only `memory.apply`.

**Why.** Before this, every certificate the CA signed was "hermes", so a second client could not have less power than the first, and the audit log could not say which of them acted. The dreamer reads other people's text (tool results and web pages inside conversations) and is model-driven, so it gets the least power that does the job: it can ask the owner, and the owner's tap is the only thing that writes memory. It cannot merge, revert, approve or touch the memory tests because no tool for any of them exists for it, and the blocked-case tests assert that each stays refused: the policy table (`internal/policy/actor_test.go`), the gateway (`internal/mcpsrv/dreamer_test.go`) and end to end over real mTLS with a throwaway CA (`test/e2e/dreamer_test.go`: a spoofed actor header, a certificate from another CA, an actor the CA signed but policy does not know, and a read of `.tests`).

**Consequences.** Not built yet: the client that uses this (`chhayactl dream run`), its cron line, the ledger of processed traces, and the batched card. The dreamer's certificate must be issued on the box (`deploy/issue-client-cert.sh dreamer deploy/certs`) before that client can connect.

**Reopen if:** the dreamer needs a tool that is not memory (then add it to the list in code with its own tests), or a third client appears (then replace the two named actors with a table).

## 2026-11-19: every job reports with a short Telegram summary and a link to a detailed page

**Decision.** A scheduled job does not end in a log file. It sends one short summary on Telegram (a headline, counts and names; never a line or quote from a conversation) and puts the detail on an HTML page in the private bucket, linked from the summary by a presigned URL. `internal/jobreport.Deliver` is the one implementation: it writes the page, uploads it to `s3://BUCKET/evals/reports/NAME.html`, presigns it, builds the summary with the link, sends it, and, if the upload or the send fails, still sends the summary and says the report is missing. The nightly eval report (`chhayactl report nightly`) and the dreamer (`chhayactl dream run -send`) both use it. A job that could not start (the dreamer cannot reach the gateway) sends a FAILED summary too, so no message at all still means the job itself did not run.

**Why.** The owner's rule, stated 2026-11-19 after the evals report worked. The nightly checks wrote to files nobody reads; the dreamer reads personal conversations and proposes memory, so what it did must be visible to its owner without opening a shell. The text stays off Telegram because the chat is a lower-trust place than the private bucket, and the page link is short-lived by design.

**Consequences.** Not yet reporting this way: `backup`, `trace export` and the cost reports (the cost report is itself Telegram messages). The morning report could list whether the backup and export ran; in `docs/backlog.md`. New jobs use `jobreport.Deliver` from the start.

**Reopen if:** the 6-hour link (role credentials) is a nuisance, or the summaries become noise.
