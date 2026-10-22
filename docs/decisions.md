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
