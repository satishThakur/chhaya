# Backlog

Feature requests deferred on purpose. Feature work is paused until deployment and the
measurement/eval tooling are in place (see `docs/decisions.md`, 2026-11-08), so nothing here
is started. Add to the bottom with a date and who asked.

## Maintenance mode (asked 2026-11-09)

The agent can put itself in a maintenance state in which it takes no conversations: for the
nightly backup (around 02:00 IST, when nobody is talking to it), upgrades and restores. Today
the backup plan simply stops the containers for a few seconds; a real maintenance state would
replace that. Likely shares the gateway's `state.json`, which already holds the kill switch and
which `chhayactl` can set without the daemon. Open: what the agent tells a user who messages
during it, and whether Hermes' own cron jobs pause.

## Backup as part of the product (asked 2026-11-09)

Backup and restore are Go now (`chhayactl backup`, decided 2026-11-09). Still open: triggering it
remotely (an EventBridge rule calling SSM Run Command, so the box needs no cron) and making
it a quiesce request to maintenance mode instead of stopping the containers.

## Self-testing memory (asked 2026-11-11)

Every fact carries a probe, a weak-model retriever proves the memory is navigable, and two
background agents (extractor, reorganizer) propose improvements through the approval flow. Full
request, how it fits the gateway, staging and open questions: `docs/memory-selftest.md`. The first
stages (fact ids and probes, the eval runner) are measurement and can follow the eval runner; the
agents are features and wait for the freeze to lift.

## Cost levers (measured 2026-11-15, not yet pulled)

First cost report: about 85k prompt tokens per model call, no caching, and the daily digest (cron) is the dearest single job. In order of expected saving; each one is measurement plus a config change, not a feature.
1. **Prompt caching.** `cache_read_tokens` is 0 everywhere. Hermes talks to `https://api.anthropic.com/v1/chat/completions` (the OpenAI-compatible endpoint), whose usage block has no cache fields; whether that endpoint caches at all, and whether Hermes has a native Anthropic transport that sets `cache_control`, is unverified. Cache reads are 10% of the input price.
2. **What fills the 85k.** The system prompt, the tool schemas and the skills prompt are re-sent on every call. The request dumps (`HERMES_DUMP_REQUESTS=1`) show the split; trim toolsets and skills the digest does not use.
3. **The digest on Haiku** (or a cheaper schedule) once it is measured against the labels.

## Route different tasks to different models or endpoints (cron half DONE 2026-11-16, see decisions.md; remainder open)

Today one model serves everything (`model.default` in `hermes/config.yaml`, from `CHHAYA_HERMES_MODEL`). Leads found in the pinned Hermes, none tried: `providers:` can hold several providers; Hermes cron jobs can pin their own model (`cron/jobs.py`); auxiliary work (titles, compression) is a separate call path with its own hooks (`pre_/post_auxiliary_call`), so it can be billed to its own model and its cost is not in the sessions table. The design question is where the rule lives: per cron job, per skill, or per message class, and the digest on Haiku with chat on Sonnet is the first case.

## Cost from Hermes hooks instead of cron (asked 2026-11-15; answer: possible, not better yet)

Hermes has plugin hooks (`on_session_end` per turn, `on_session_finalize`, `on_session_reset`, `post_api_request` with per-call `usage`) and shell hooks and outbound webhooks configured in `config.yaml`. Why the cron stays: (1) a session that is only abandoned never fires a hook, since our idle reset runs on the next message; the cron's quiet-for-35-minutes rule sees it; (2) a hook runs inside the Hermes container, which must not hold the approver bot token, so it would have to call the gateway, and a bill the agent's own process sends is one it could suppress. What hooks would add: `post_api_request` gives per-call usage, which is the way to see cache hits per call and to enforce a per-session spend cap. Revisit when a cap is wanted. The same payload shows `api_mode: "anthropic_messages"`: Hermes has a native Anthropic mode, and our config uses `transport: chat_completions`. That is the first thing to try for prompt caching.

## Send the nightly check results to Telegram (asked 2026-11-16)

`trace check` now runs nightly into `~/.chhaya/backups/check.log` on the box, so a `fail` is only seen if someone reads the log. The cost report already sends from the approver bot; do the same for a summary line ("9 traces, 2 flags, 0 fails") plus any `fail`. Needs the check's `-json` output and a send path in `chhayactl`; not built.

## Dreaming pipeline: nightly memory extractor (design only, asked 2026-11-16)

Design in `docs/dreaming.md`. Blocked on: a baseline from the nightly checks, a smaller trace-replay eval runner, and a second gateway client (`dreamer`) with its own policy.
