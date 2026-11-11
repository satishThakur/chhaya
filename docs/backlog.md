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
