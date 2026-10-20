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

**Also settled here:** `sampling: false` and `elicitation: false` on the MCP server. Both default to **on** in Hermes. Elicitation would surface approval prompts as Telegram buttons *through the agent* — the exact thing the separate approver bot exists to avoid.

---

## 2026-10-20 — `skills.write_approval: true` is permanent, not "at first"

The first draft treated this as early-days caution to relax later.

**Why it changed:** a Hermes skill that declares `required_environment_variables` in its frontmatter gets those variables forwarded into the sandbox **automatically, with no config change**. Hermes authors its own skills. That is a path from "the agent wrote a skill" to "a secret is in the sandbox" that never crosses my desk. Skill write approval is what closes it.

Related: `HERMES_WRITE_SAFE_ROOT` is set, and `terminal.env_passthrough` is explicitly empty alongside `docker_forward_env`.

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
