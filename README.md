# Chhaya

A personal chief-of-staff agent, reached over Telegram.

[Hermes](https://github.com/NousResearch/hermes-agent) runs the agent loop. Everything in this repo is the part
we own: a policy gateway that sits between the agent and anything that matters, and the Personal memory the
agent reads from.

**The one rule: Hermes decides *what* to do; the gateway decides *whether it's allowed*.** The agent can edit
its own skills and memory, so it must not be able to edit its own permissions. The gateway is a separate
service, outside the agent, and every tool call goes through it.

## How it fits together

```
You ── Telegram ──▶ Hermes (agent loop)
                       │  one MCP server, HTTPS + mTLS
                       ▼
                  chhayad (policy gateway) ──▶ tools: memory, and later Swiggy, Claude worker, Garmin
                       │
You ◀── Telegram ──▶ approver bot: approval cards, /stop, /resume, /status
```

- **Tiers.** Every tool is registered with a tier: 0 internal, 1 reversible, 2 money or data leaving the
  box. Tier 3 (irreversible) is never exposed. A tool with no tier is a bug.
- **Policy is code.** The rules live in `internal/policy/`. `config/policy.yaml` holds only values such as
  caps, allowlists and timeouts. A missing value is read as the strictest one.
- **Kill switch.** `/stop` on the approver bot refuses every write until `/resume`. It is on the approver
  bot, not Hermes' bot, so the switch is not routed through the process it stops.
- **Taint.** Once untrusted content enters a session, every write needs approval.
- **Audit.** Every decision is appended to a hash-chained log. `chhayactl audit verify` walks the chain.
- **Fail closed.** If the gateway cannot work out a tier, read its own state, write its audit log or reach
  the approver, it refuses.

## Personal memory

`memory/` is a folder of Markdown documents, tracked in git: tastes, standards, people, decisions. The agent
reads them with `memory.get`. It cannot write them.

To remember something, the agent calls `memory.propose_update` with one literal line, for example
`No okra.`. The approver bot posts a card:

```
approve memory?
taste/food: No okra.
why: User said "I do not like okra"
[✅ approve] [❌ reject]
```

Tap approve and the gateway appends `- No okra. <!-- proposal <id>, approved <date> -->` to
`memory/taste/food.md`. You review the change and commit it. Hermes plays no part between the proposal and
the write. A tap is refused while `/stop` is on, or if the proposal changed after its card was posted.

## Running it locally

Needs Docker, Go 1.24+, and two Telegram bots: one for Hermes, one for the approver.

```bash
cp deploy/.env.hermes.example  deploy/.env.hermes    # Hermes' bot token, model settings
cp deploy/.env.gateway.example deploy/.env.gateway   # approver bot token, your Telegram user id
make dev-doctor   # checks env files, tokens and certs; changes nothing
make dev-certs    # once: CA plus gateway and Hermes certificates
make dev-up       # preflight, then build and start both containers
```

Keep the two env files separate. If the approver token ever reaches Hermes, the agent could approve its own
requests. `chhayad` refuses to start if it can see Hermes' token.

`make help` lists every verb. [`docs/runbook.md`](docs/runbook.md) has the full walkthrough and a smoke test.

## Development

```bash
make test     # unit tests, every package
make evals    # L1 safety evals (internal/policy)
make e2e      # the blocked-write exit check, over real mTLS
make build    # chhayad and chhayactl into bin/
```

Both `make test` and `make evals` must pass before a change is done. Any change to `internal/policy/` needs a
test proving that a blocked case stays blocked.

## Layout

```
cmd/chhayad/        the gateway daemon (wiring only)
cmd/chhayactl/      operator CLI: audit verify, stop
internal/policy/    tiers, caps, taint, kill switch (pure, no I/O)
internal/audit/     hash-chained append-only log and its verifier
internal/approval/  the approver bot: approval cards, memory cards, /stop
internal/mcpsrv/    MCP over HTTPS + mTLS; the only path to a tool handler
internal/memory/    memory proposals: validation, staging, the approved append
internal/tools/     tool handlers
config/policy.yaml  caps, allowlists, timeouts
memory/             Personal memory (Markdown, git-tracked)
hermes/             Hermes config template and SOUL.md
deploy/             docker-compose, certificates
docs/               design, decisions, runbook, specs and plans
```

## Read before changing things

- [`docs/design.md`](docs/design.md): what this is and why it is shaped this way.
- [`docs/decisions.md`](docs/decisions.md): what was decided, and what evidence would reopen it. Several
  choices look like friction but carry weight. Read the entry before simplifying one.
- [`CLAUDE.md`](CLAUDE.md): the non-negotiables, for humans and agents alike.
