# CLAUDE.md

Chhaya is a personal chief-of-staff agent. [Hermes](https://github.com/NousResearch/hermes-agent) is an adopted dependency that provides the agent loop; everything in this repo is ours.

Read `docs/design.md` before non-trivial work. Read `docs/decisions.md` before re-opening a settled question — it records what was decided and what evidence would reopen it.

## The one rule

**Hermes decides *what* to do; the gateway decides *whether it's allowed*.**

The gateway is the security boundary, and it is outside the agent on purpose. Hermes writes its own skills and memory, so an agent that can edit itself must not be able to edit its own permissions.

## Non-negotiables

- **Every tool is registered with a tier, a validator and an audit entry.** No tool bypasses `policy/`. A tool with no tier is a bug, not a default.
- **Policy is code plus `config/policy.yaml` — never prompt text.** A limit stated only in a tool description is not enforced. Tool descriptions may *describe* limits so the agent plans well; `policy/` is what holds them.
- **Changes to `policy/` need tests that prove a blocked case stays blocked.** Write the failing test first. A policy change with only happy-path tests does not land.
- **Secrets come from env only.** Never log them. The audit logger redacts by default — don't add a path around it.
- **Never edit files under `memory/` directly.** It changes only through `memory.propose_update` flows, and every entry records its provenance.
- **Fail closed.** If the gateway cannot determine a tier, read its own policy state, or reach the approver, it refuses the write.

## Stack and workflow

- Python 3.11+, `uv`, `pytest`, type hints, small focused modules.
- `make test` and `make evals` must pass before saying done. Run them; don't predict them.
- Hermes is **pinned to a release tag** and upgraded deliberately after reading the changelog. Never track `main`. (v0.22.1 shipped from the wrong commit — this is why.)
- Branch for feature work. The agent's own worker is restricted to `agent/*` branches and never merges to `main`.
- Append to `docs/decisions.md` whenever we make a choice, so later sessions don't re-litigate it.

## Layout

```
gateway/          MCP server Hermes talks to — the security boundary
  policy/         classifier, tiers, caps, taint, kill switch
  approvals/      chhaya-approver Telegram bot (and /stop)
  audit/          append-only log
  tools/          memory, swiggy, claude_worker, garmin
sync/garmin/      scheduled pull into SQLite
memory/           Personal memory (Markdown, git-tracked) — propose_update only
hermes/           config.yaml template, SOUL.md, skills we author
config/policy.yaml  caps, allowlists, repo list
evals/            L0 ops, L1 safety, L2 failure modes, L3 taste
deploy/           docker-compose.yml now, AWS later
docs/             design.md, decisions.md, diagrams/
```

## Decisions that look like friction but are load-bearing

A future session will be tempted to "simplify" each of these. Don't, without reading the entry in `docs/decisions.md` first.

- **The gateway is a standalone HTTPS+mTLS service, not a stdio MCP subprocess.** As a child process, Hermes would own the lifecycle of the process holding the kill switch, and Hermes strips credential-named env vars from MCP subprocesses — so the gateway's secrets would have to live in Hermes' config.
- **`/stop` is on the approver bot, not Hermes' bot.** A kill switch routed through the process it stops is not a kill switch.
- **MCP `sampling` and `elicitation` are disabled.** Both default to on. Elicitation would surface approval prompts through the agent, defeating the separate approver bot.
- **`skills.write_approval: true` is permanent.** A skill can declare `required_environment_variables` and have them forwarded into the sandbox automatically, with no config change. Approval on skill writes is what closes that path.
- **`/new` at task boundaries.** It refreshes memory *and* keeps one task equal to one trace, because the trace correlation id is a conversation id.

## Tone

Match the design doc: plain sentences, concrete nouns, no hedging. When something is unverified, say so and say what would verify it.
