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
