# Runbook: running chhaya locally

Two containers. The gateway owns its own lifecycle and holds every credential;
Hermes holds exactly one secret, the token for its own Telegram bot, and reaches
everything else through the gateway over mTLS.

`make help` lists every verb. The logic lives in `scripts/dev.sh`; the Makefile is
just where the verbs are discoverable.

---

## First time

**1. Fill in `deploy/.env.hermes` and `deploy/.env.gateway`.**

Copy from the `.example` files if they do not exist yet. The split is not
cosmetic — it is the whole safety model:

| File | Holds | Must never hold |
|---|---|---|
| `.env.hermes` | `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ALLOWED_USERS`, the model settings and `PROXY_API_KEY` | the approver token, or any downstream credential |
| `.env.gateway` | `CHHAYA_APPROVER_BOT_TOKEN`, `CHHAYA_APPROVER_ALLOWED_USERS`, Swiggy/GitHub/Garmin | `TELEGRAM_BOT_TOKEN` |

If the approver token ever reaches Hermes, the agent can forge its own approvals
and press its own kill switch, and the rest of the design is decorative. `chhayad`
refuses to start if it can see `TELEGRAM_BOT_TOKEN`; `make dev-doctor` checks both
directions before anything starts.

**M0b does not include the logging proxy**, so for a live test point Hermes at a
provider directly and accept that you get no traces:

```
CHHAYA_HERMES_MODEL=claude-sonnet-5
CHHAYA_HERMES_API_BASE=https://api.anthropic.com/v1
PROXY_API_KEY=<the key for that endpoint>
```

**2. Generate certificates** — once, then never again unless you mean to rotate:

```bash
make dev-certs
```

Writes a CA plus a server and a client certificate to `deploy/certs/`. Nothing
there is ever committed; `.gitignore` and `.dockerignore` both exclude it.

**3. Check before starting:**

```bash
make dev-doctor
```

Changes nothing. Reports missing env values by name, never by value.

**4. Press Start on both bots in Telegram.** A bot cannot open a conversation
with you — until you message it first, `sendMessage` fails with `chat not found`
even though the token is valid. The gateway probes the approver chat at startup
and refuses to start if it cannot reach it, so skipping this looks like a
configuration error.

**5. Start:**

```bash
make dev-up
```

Preflights, renders Hermes' config, builds the gateway image and starts both.

---

## The smoke test

Run this from Telegram against **Hermes'** bot. It is the acceptance test for
M0b, taken from the design doc, plus one probe the review added.

| # | Do this | Expect |
|---|---|---|
| 1 | "ping the gateway" | A pong with the gateway version. Proves mTLS, MCP and the tool path. |
| 2 | "what do you know about my food taste?" | The contents of `memory/taste/food.md`. Proves a tier-0 read. |
| 3 | "what can you do right now?" | The `capabilities` tool: caps remaining, kill switch off, taint clear. |
| 4 | "remember that I don't eat mushrooms" | A **proposal id**, and `memory/taste/food.md` unchanged on disk. `propose_update` never edits a document. |
| 5 | Send `/stop` to the **approver** bot | "stopped. Writes are refused." |
| 6 | Retry step 4 | Refused, and the agent can say why — the kill switch is active. |
| 7 | "fetch https://example.com and tell me what it says" | **This is a probe, not a feature.** If the agent can fetch it, Hermes' web toolset is enabled and the taint model is blind to the likeliest injection vector. See `docs/bugs.md` B6 — if this succeeds, stop and tighten `toolsets` before trusting taint. |
| 8 | Send `/resume` to the approver bot | "resumed." |
| 9 | Retry step 4 | Succeeds, returns a proposal id. |

Then confirm the record:

```bash
make dev-audit      # tail it live while you run the steps above
make dev-verify     # walk the hash chain from genesis
```

`dev-verify` must report the chain intact. Every one of the steps above should
appear — including the refusal at step 6, with rule `killswitch.active`. **That
refusal being present in the log is the milestone.** Everything else in M0b exists
to make that one line trustworthy.

---

## Day to day

```bash
make dev-status     # what is running, plus an mTLS probe from both sides
make dev-logs       # follow both; `scripts/dev.sh logs gateway` for one
make dev-audit      # watch decisions as they happen
make dev-down       # stop
```

`make dev-status` deliberately reports success when an **uncertificated** client is
*refused*. A gateway that answers without a client certificate is the failure.

---

## The kill switch

Three paths, in descending order of trust:

1. **`/stop` on the approver bot.** The primary. Hermes has no token for that bot,
   so the agent can neither see nor press it.
2. **`make dev-stop`** (`chhayactl stop` inside the container). The backstop for
   when the bot's poll loop is dead and `/stop` cannot be delivered.
   *Note `docs/bugs.md` B1: under a busy daemon this can fail loudly and need a
   retry. It will never silently succeed.*
3. **`make dev-down`.** Removes Hermes' only route to anything.

Reads keep working while stopped, on purpose — a gateway that cannot answer
"why are you refusing?" while refusing is worse than one that can.

---

## When something is wrong

| Symptom | Likely cause |
|---|---|
| `chhayad` exits immediately naming `TELEGRAM_BOT_TOKEN` | `.env.hermes` was mounted into the gateway. That check is the one that must never be bypassed. |
| Gateway refuses to start: approver chat unreachable | You have not pressed Start on the approver bot, or the chat id is wrong. |
| Gateway refuses to start: broken chain | The audit log does not verify. Do not delete it — that is the evidence. Read it first. |
| Hermes starts but has no tools | Check `deploy/.hermes-config.rendered.yaml` really contains the `mcp_servers` block and no `${...}` placeholders. |
| Every write needs approval unexpectedly | The session is tainted. `/clear` on the approver bot. |
| Everything is refused with `audit.unwritable` | The audit log became unwritable and the gateway latched. That is deliberate: if it cannot log, it will not act. Fix the disk, then restart. |
