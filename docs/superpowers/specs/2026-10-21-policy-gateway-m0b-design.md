# Policy Gateway (M0b) — Design

Oct 21, 2026 · status: approved for planning

**Parent spec:** [`docs/design.md`](../../design.md) — the project design.
**Decision log:** [`docs/decisions.md`](../../decisions.md) — every constraint below traces to an entry there.

## 1. What this builds

The policy gateway skeleton: every safety mechanism real and exercised, with nothing valuable yet behind it. Four stub tools, none of which can do harm, passing through a policy engine, an approver bot and a tamper-evident audit log that are all production-shaped.

The order is deliberate. We prove the boundary holds *before* putting code, money or health data on the other side of it.

**Exit check (from `design.md`):** a blocked write is logged.

Made concrete: `/stop` on the approver bot → call `memory.propose_update` → `Deny(killswitch.active)` → an audit record exists → `chhayactl audit verify` passes. One flow exercising the kill switch, the policy engine, the refusal path and the hash chain, using only real tools.

### Out of scope for M0b

Swiggy, the Claude worker, Garmin, the real Personal memory store, Langfuse, the model proxy, cron. Tier-1 and tier-2 tools: the engine must *decide* them correctly, but no tool registers above tier 0 yet. Per-session taint (see §4.3).

## 2. Inherited constraints

Settled already; this spec does not relitigate them.

| Constraint | Source |
|---|---|
| Standalone HTTPS+mTLS service, never a Hermes subprocess | decisions.md, 2026-10-20 |
| `/stop` and `/resume` on the approver bot, not Hermes' bot | decisions.md, 2026-10-20 |
| Code decides policy; `policy.yaml` supplies values only | this session |
| Refuse to start if the approver chat is unreachable | decisions.md, 2026-10-21 |
| Hermes pinned by digest `sha256:1d33f6087fab79d580ffaec34ceed825e11d7bb8e803206be75a56d027c62538` (v0.22.3) | decisions.md, 2026-10-21 |
| Go for everything we write | this session |
| Audit log is tamper-evident | this session |

## 3. Architecture

### 3.1 Two binaries

- **`chhayad`** — the gateway daemon.
- **`chhayactl`** — local CLI: `audit verify`, `policy explain <tool>`, `stop`. Separate so that auditing the log never runs inside the process that writes it, and so the kill switch has a path that does not depend on the daemon being healthy.

Module path `github.com/satishthakur/chhaya`; `go 1.24` in `go.mod`, matching the installed toolchain and upgraded deliberately, as with the Hermes pin. (Go 1.27.3 is current; the version is one line to change.)

### 3.2 Package layout

```
cmd/chhayad/        wiring only, no logic
cmd/chhayactl/      audit verify, policy explain, stop
internal/policy/    PURE: tiers, taint, caps, Decide(). No I/O, no clock, no network.
internal/audit/     hash-chained append-only writer + independent verifier
internal/approval/  approver bot: cards, /stop, /resume, /clear
internal/mcpsrv/    MCP over HTTPS+mTLS, tool registry, Invoke()
internal/tools/     ping, memory.get, memory.propose_update, capabilities
internal/config/    policy.yaml load + validate
```

This supersedes the Python-flavoured tree in `design.md` §"Repo layout", which must be revised (§9).

### 3.3 The bypass-proof invariant

`CLAUDE.md` requires that no tool bypasses policy. In Go this is structural rather than a review convention.

The tool registry is unexported inside `mcpsrv`. The only exported path to a tool handler is:

```go
func (g *Gateway) Invoke(ctx context.Context, call ToolCall) ToolResult
```

`Invoke` is the sole caller of the registry and always runs, in order: `policy.Decide` → dispatch-or-refuse → `audit.Append`. Tools register as data. There is no exported function that reaches a handler directly, so adding a bypass means editing `mcpsrv` itself — a reviewable event, not an easy accident.

A test asserts the invariant directly: every registered tool name must be unreachable except through `Invoke`.

### 3.4 Transport

`github.com/modelcontextprotocol/go-sdk` v1.8.0 (stable). `StreamableHTTPHandler` implements `http.Handler`, so mTLS is ordinary `tls.Config` on `http.Server`:

```go
tls.Config{
    ClientAuth: tls.RequireAndVerifyClientCert,
    ClientCAs:  ourCAPool,
    MinVersion: tls.VersionTLS13,
}
```

Verified working end to end during the M0a spike, including the negative control.

MCP `sampling` and `elicitation` are disabled on Hermes' side (`{enabled: false}` — nested dicts, not booleans; see decisions.md). The gateway additionally refuses sampling requests if one ever arrives, rather than trusting config alone.

## 4. Data models

### 4.1 Policy

```go
type Tier int    // 0 internal · 1 reversible · 2 money/leaves box · 3 irreversible
type Kind int    // Read | Write
type Outcome int // Allow | Deny | NeedApproval

type Request struct {
    Tool string
    Args map[string]any
    Kind Kind
    Tier Tier // from registration, never from the caller
}

type State struct {
    Tainted     bool
    TaintedAt   time.Time
    TaintSource string
    KillSwitch  bool
    KilledAt    time.Time
    Spent       map[string]int64 // minor units, per cap key, per day
    Calls       map[string]int   // rate-limit counters
}

type Decision struct {
    Outcome Outcome
    Tier    Tier
    RuleID  string   // "killswitch.active", "cap.food.daily" — in card AND audit
    Reason  string   // agent-readable: "blocked: over daily cap; ₹420 left"
    Redact  []string // arg paths to scrub before auditing
}

func Decide(req Request, st State, cfg Config, now time.Time) Decision
```

Pure: no I/O, no ambient clock, no network. The clock is injected because caps need it. Every safety property is therefore a table test with no mocks.

**Tier comes from the tool's registration.** A caller cannot propose its own tier.

### 4.2 Evaluation order

The order *is* the security property, so it is explicit and exhaustively tested:

1. unknown tool → **Deny** (`tool.unknown`)
2. kill switch ∧ write → **Deny** (`killswitch.active`)
3. tier 3 → **Deny** (`tier3.not_exposed`) — and never registered
4. tainted ∧ write → **NeedApproval** (`taint.write`), whatever the tier
5. tier 0 → Allow · tier 1 → Allow if validators pass · tier 2 → NeedApproval
6. cap or rate limit exceeded → **Deny** (`cap.*`, `ratelimit.*`), remainder in `Reason`

Reads are never blocked by the kill switch: `design.md` scopes it to writes, and a gateway that cannot answer `capabilities()` while stopped cannot explain why it is stopped.

### 4.3 Taint

**Global boolean, not per-session.** Set when any gateway tool returns untrusted content; cleared only by `/clear` on the approver bot.

Two findings drove this:

- **The gateway cannot see Hermes' session id.** `identity_header` is HTTP-only and carries a per-profile digest; nothing in the MCP call path carries a conversation id. Hermes holds one long-lived connection spanning many Telegram sessions.
- **A model-supplied session id would be worse than none.** An injected agent emits a fresh id and walks out of its own quarantine. Never let the thing being constrained supply the constraint key.

Global taint fails in the safe direction: stale taint costs extra approvals, never fewer.

**Precondition:** Hermes' built-in web/browser/fetch toolsets are disabled, so every untrusted read passes through the gateway. Without this, taint tracking is blind to the likeliest injection vector while diligently tracking the ones it can see. Any future web access arrives as a gateway tool that marks results untrusted.

**Upgrade path:** when the trace proxy lands, it sees `session_affinity_header` on every LLM call and can push session boundaries to the gateway — genuinely per-session, still unforgeable. Moving to it changes no tool schema.

### 4.4 Tool schemas

| Tool | Tier | Kind | Args | Returns |
|---|---|---|---|---|
| `ping` | 0 | read | — | `{pong, version, time}` |
| `memory.get` | 0 | read | `{path}` | markdown, marked trusted |
| `memory.propose_update` | 0 | write | `{path, diff, rationale}` | `{proposal_id}` |
| `capabilities` | 0 | read | — | caps left, taint, kill switch, per-tool limits |

Every tool result carries an explicit `trust` field (`"trusted"` or `"untrusted"`), set by the gateway from the tool's registration — never by the tool handler and never by the caller. An `untrusted` result sets the taint flag as a side effect of returning it. In M0b every tool is `trusted`, so no result can set taint; the field and the wiring exist so that the first untrusted source added later is covered by construction rather than by remembering.

Every tool's description states its limits, so the agent plans within them rather than discovering them by rejection.

`capabilities` exists so Hermes can answer "can you order dinner now?" honestly instead of guessing.

`memory.propose_update` writes to a stub store in M0b. It earns its place because the exit check needs a write to block; using a real tool avoids a synthetic `debug.write_probe` that would exist only for tests.

### 4.5 Audit record

```json
{ "seq": 42, "ts": "2026-10-21T18:04:11.123Z",
  "prev": "sha256:…", "hash": "sha256:…",
  "actor": "hermes", "tool": "memory.propose_update", "kind": "write", "tier": 0,
  "args": {"path": "taste/food", "diff": "<redacted:2048B>"},
  "decision": "deny", "rule": "killswitch.active",
  "reason": "kill switch active since 2026-10-21T17:40:02Z",
  "taint": {"tainted": false}, "approval": null,
  "result": {"status": "refused"}, "duration_ms": 3 }
```

`hash = sha256(canonical_json(record_without_hash) ‖ prev)`, canonical being sorted-key compact JSON so it reproduces byte-for-byte. Genesis record has `prev: null`.

**Redaction happens before hashing.** Secrets never enter the log and therefore never enter the chain.

**Approvals are two records, not one:** the request (`needs_approval`) and the answer (`approved` / `denied_by_user` / `timeout`), the second carrying `latency_ms`. An append-only log should not have mutable rows, and a timeout needs a trace as loud as a tap. `latency_ms` is deliberate — `design.md` tracks approval fatigue by time-to-approve.

## 5. Approver bot

Long-polls Telegram with the gateway's own token. Hermes has no token for this bot, which is what makes approvals and the kill switch unforgeable.

**Commands:** `/stop` (kill switch on, persisted), `/resume` (off), `/clear` (clear taint), `/status` (kill switch, taint, caps remaining).

**Approval cards** state what, why, cost, and which rule fired, with ✅/❌ buttons. Verified live in the M0a spike: the tap returns a `callback_query` carrying the rule id, and `answerCallbackQuery` closes the loop.

**Only the configured user id is honoured.** Any other sender is ignored and audited.

**Timeout means deny**, and the timeout is its own audit record.

## 6. Startup sequence

Fail-closed and ordered; each step names the decision it defends.

1. Load and validate `policy.yaml`. Reject unknown keys. **A missing or unparseable value resolves to the most restrictive interpretation, never the most permissive** — a missing cap is zero, not infinity. A typo can only tighten the boundary.
2. Open the audit log, verify the chain tail. A broken chain is a refusal, not a warning.
3. `getMe` on the approver bot. Bad token, no start.
4. `getChat` on the configured chat. Unreachable → exit non-zero.
5. Send a startup card **only if the policy hash changed or this is first boot.** Not on every restart: a card you see on every bounce is one you stop reading, and this bot's messages must stay worth reading.
6. Load persisted kill-switch state. A `/stop` survives a restart.
7. Only now bind the mTLS listener.

Hermes may connect before step 7 and will retry. That ordering is correct: a gateway that accepts calls before it can ask for approval is a gateway that fails open.

**The blocked-bot gap.** A user who blocks the bot still passes `getChat` but fails `sendMessage`. So if a real approval send fails, the gateway writes a CRITICAL audit record, refuses the action, and returns a structured error telling Hermes the approval channel is down. The agent cannot forge an approval, but it can tell the user in chat that it cannot reach the approver bot — the agent becomes the out-of-band alarm precisely when the safety channel is what is broken.

## 7. Configuration

**`config/policy.yaml`** — values only, no rules:

```yaml
caps:
  food: {per_order_inr: 800, per_day_inr: 1500}
  rate_limits: {default_per_hour: 60}
allowlists:
  repos: []        # filled at M2; empty means no repo is writable, which is the safe default
  branch_prefix: "agent/"
approvals:
  timeout_seconds: 300
```

**Secrets from env only**, via `deploy/.env.gateway`: `CHHAYA_APPROVER_BOT_TOKEN`, `CHHAYA_APPROVER_ALLOWED_USERS`. Never in `policy.yaml`, never logged.

**The gateway refuses to start if `TELEGRAM_BOT_TOKEN` is set in its own environment.** That variable belongs to Hermes; its presence here means the wrong env file was mounted, and the two bots' secrets have been crossed. The check needs no knowledge of the token's value — presence alone is the error. It also refuses if `CHHAYA_APPROVER_BOT_TOKEN` is absent or equals `TELEGRAM_BOT_TOKEN`.

A rule enforced in code beats a rule written in a comment — the project's thesis applied to its own setup. This is the one configuration mistake that would silently void the safety model, so it is the one that must fail loudly at boot.

## 8. Testing

**`internal/policy` — table tests over the full decision order.** Every rule, every ordering interaction. Properties worth asserting directly: no input yields `Allow` for a write while the kill switch is on; no input yields `Allow` for a write while tainted; tier never comes from `Args`. These *are* the L1 safety evals from `design.md`; they are not a separate artefact.

**`internal/audit`** — chain verifies on a clean log; mutating any past record fails verification; truncation fails verification; redaction happens before hashing (assert no secret appears in the hashed bytes).

**`internal/approval`** — fake Telegram transport. Timeout yields deny plus its own record. An unauthorised sender is ignored and audited. A failed send yields CRITICAL plus refusal.

**`internal/mcpsrv`** — a client without a certificate is refused; the registry is unreachable except via `Invoke`.

**End-to-end** — compose up, real mTLS MCP client, `/stop`, `memory.propose_update`, assert deny, assert the audit record, assert `chhayactl audit verify` passes.

**One rule, learned the hard way during the spike:** assert on behaviour or structure, never on wording. A check that string-matched a Hermes comment went red when the comment was rephrased with no change in meaning. A test that fails on rewording trains you to ignore it, and a safety test you have learned to ignore is worse than no test.

## 9. Documentation changes this requires

- `CLAUDE.md` — the stack section says "Python 3.11+, `uv`, `pytest`". Replace with Go, and update the layout block.
- `docs/design.md` — the repo-layout tree lists `.py` files; the Hermes bootstrap section should note the disabled web toolsets as architectural rather than incidental.
- `scripts/verify-hermes-image.sh` — convert the source-inspection checks from `python3 -c inspect.getsource` to plain `grep` against files in the image. Python stays only where we must execute Hermes' own code (the import and affinity-stamping checks), which is probing a Python system rather than writing Python.

## 10. Deferred, with the trigger that reopens each

| Deferred | Reopen when |
|---|---|
| Per-session taint via the trace proxy | the proxy exists (M0 tracing) |
| Pre-approved patterns mined from approval history | approval fatigue is measurable, i.e. after real usage |
| SQLite index over the audit log | "what did it do yesterday?" becomes slow to answer by grep |
| Tier-1 and tier-2 tools | M2 (code worker) and M3 (food) respectively |
| Web access as a gateway tool | a job actually needs it; not before |
