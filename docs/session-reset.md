# Session reset policy

**Status:** reviewed 2026-11-12; installed and verified on the desktop stack the same day; **not yet on the box**. Related: `docs/bugs.md` B10 (Telegram threads), `docs/observability-and-evals.md`
(a trace is one conversation).

## The problem

Hermes keeps a Telegram chat as one session until `/new`. Core stopped rotating sessions on a timer
(upstream commit `1d5d059410`, v2026.9.11), and `hermes doctor` says so: *gateway conversations reset only
on /new or /reset*. Sessions therefore run for days and mix unrelated work. That costs money (every model
call re-sends the whole history; one session used 1.76M input tokens), shows the model unrelated history on
every call, leaves memory stale (memory is read at session start, so a fact approved this morning is not
visible in a session begun two days ago), and makes traces hard to annotate. The export now splits
sessions into conversations at 60 minutes of silence (`decisions.md`, 2026-11-12), which fixes measurement
but not the agent.

## The plugin

`hermes-session-reset-policy`, a community plugin by one author (MIT), in the Hermes catalog pinned at commit
`0f2ea5ad995f1e4ba7c92c2dbd21afa868b8ae21` (the catalog installs that commit, and Hermes treats a catalog pin
as reviewed). It restores the idle and daily reset that core removed, reading the existing `session_reset`
block (`mode: idle | daily | both | none`, `idle_minutes`, `at_hour`).

**What I read** (the repository at that commit, cloned and read, never run): `__init__.py` (431 lines),
`plugin.yaml`, the README. I did not read its 822-line test file or the Hermes functions it calls.

**What it does.** One hook, `pre_gateway_dispatch`. On each inbound message that is not a slash command it
looks up the conversation's route, compares now with the time of the owner's last message (kept as one
metadata key, `srp_last_user_inbound`, written through Hermes' own `set_session_metadata`), and if the idle
limit has passed and no turn is running, ends the session through the same path a typed `/new` takes
(`gateway._handle_reset_command`). The message then lands in the fresh session. No timers, no background
work. A turn in flight is never reset; slash commands and cron traffic neither count as activity nor
trigger a reset.

**What it does not do.** No network, no subprocess, no file or environment access, no `eval` (searched, none
found); standard library only, with Hermes internals imported lazily. It writes nothing but that one metadata
key, through Hermes.

**Findings that matter**
1. **It fails open and quietly.** Any error and it carries on with the existing session, logged at debug or
   warning. A Hermes upgrade that renames the private functions it relies on (`_handle_reset_command`,
   `_session_key_for_source`) would silently end resets, apart from a degraded fallback to a bare store
   rotation. We therefore verify with data, not logs: the `session.not_reset` check flags a conversation that
   began with your message after more than an hour of silence in the same session. Re-run it after every
   Hermes upgrade.
2. **A reset is not announced to the agent.** It behaves like `/new`: the agent gets no "context was reset"
   note. First-turn skill injection still applies.
3. **It resets on your next message, not on a clock.** A session idle for hours is closed the moment you write.
   Cron deliveries do not keep it alive.
4. **`at_hour` uses the gateway's local time**, probably UTC in the container. Use `mode: idle` only; daily
   adds nothing at 30 minutes.
5. **Single community maintainer.** Pinned, readable in one sitting, and removable. Updating it is a deliberate
   act: re-read the diff.

## The trade-off to decide: replies to the digest

The morning digest is delivered into the chat session as a `[Cron delivery: …]` message. With a 30-minute idle
limit, if you reply to the digest an hour later, your message is the first past the boundary: the session is
reset and the agent no longer has the digest in its context ("the second item" means nothing). Options, none
verified: a longer limit (for example 120 minutes); relying on Hermes' `session_search` tool (it exists; whether
the agent uses it for this is untested); delivering the digest into its own topic (see below), which does not
fix the idle reset by itself. The new check cannot see this; annotation will. Start at 30 as chosen and watch
for conversations that open with "which one?" or "what are you talking about?".

## Telegram topics

What the pinned Hermes does is written up in `docs/bugs.md` B10. In short: with `/topic` on, each topic is an
independent session, the root chat is a command lobby, and by default a root-chat message is answered in the
most recently bound topic. A reset is per session key, and the key includes the topic, so each topic has its
own idle clock. Topics and an idle reset combine without conflict: topics give explicit task boundaries when
you want them, the idle reset ends what you leave behind. Topics cost a habit (open "All Messages", write
there) and need `/topic` once; the idle reset costs nothing. Recommendation: install the reset first, leave
topics off, and revisit topics once the digest has its own destination.

## Proposed config

In `hermes/config.yaml` (the repository template; the file Hermes reads is **re-rendered from it on every
`make dev-up`**, so `hermes plugins enable` alone would be overwritten):

```yaml
session_reset:
  mode: idle
  idle_minutes: 30
plugins:
  enabled:
    - hermes-session-reset-policy
```

## Steps (steps 1 to 4 verified on the desktop, 2026-11-12; the box is next)

1. Install the pinned commit. **Do this before the config enables it**, or use the second form.
   Before the config names it: `docker exec deploy-hermes-1 hermes plugins install
   hermes-session-reset-policy --ref 0f2ea5ad995f1e4ba7c92c2dbd21afa868b8ae21 --no-enable --no-deps`.
   Once `hermes/config.yaml` enables it (any machine that has pulled this repo and run `make dev-up`),
   Hermes counts it as active and refuses `--no-deps` ("cannot replace an active plugin"); use
   `--yes-deps --no-enable` instead. The plugin has no runtime dependencies, so the consent is for nothing.
   (Found on the box, 2026-11-12: the install printed that error and `hermes plugins list --no-bundled`
   showed nothing.) It installs under
   the Hermes data directory (`~/.chhaya/hermes/plugins`), which survives container recreation and is in the
   backup. Hermes scans the plugin and asks for consent to its declared hook.
2. Add the config above to `hermes/config.yaml`; `make dev-up`, then **`docker restart deploy-hermes-1`**.
   `dev-up` re-renders the file but does not recreate Hermes when only that file changed, so Hermes keeps the
   config it started with.
3. Check it is loaded: `docker exec deploy-hermes-1 hermes plugins list --plain --no-bundled` shows
   `enabled  catalog:community@0f2ea5ad  0.3.0`. The log will not say `armed`: Hermes logs at warning level and
   the plugin logs at info, so the log cannot confirm it.
4. Test by behaviour: message the bot, wait longer than the limit, message again, and read the sessions
   (`sqlite3 -readonly ~/.chhaya/hermes/state.db "select id, datetime(ended_at,'unixepoch'), end_reason, parent_session_id from sessions where source='telegram' order by started_at desc limit 4"`).
   The old session should have `end_reason = session_reset` and the new one should name it as its parent.
5. Repeat on the box after a `git pull`, then confirm with `chhayactl trace export` and `trace check`:
   no new conversation should fire `session.not_reset`.

**Rollback:** remove the two config blocks, `hermes plugins remove hermes-session-reset-policy`, `make dev-up`.

## Desktop test result (2026-11-12, limit temporarily 2 minutes, restored to 30)

- 16:23:00, a message after 3 days of silence: the old session `3f13f8fd` ended at 16:23:01 with
  `end_reason = session_reset`, and a new session began with it as parent.
- 16:27:11, a message 4 minutes later: that session ended at 16:27:13 (`session_reset`) and another began.
- In the new session the owner asked "what was my last message?". With no history, the agent used `tool_search`
  to find `session_search`, browsed the previous session, read it and answered correctly, in 5 tool calls and
  about 10 seconds. So a reset does not leave the agent amnesiac **when asked about earlier context**. An implicit
  reference ("the second item") gives it no reason to search and is untested.
- `session.not_reset` fired on none of the day's three Telegram conversations.

## Open

- Idle limit: 30 minutes as decided; revisit after a week of annotation (see the trade-off above).
- Whether to deliver the digest into its own topic, and whether that needs topic mode on for the whole chat.
- The upgrade checklist (`docs/ship-a-change.md`) should gain "re-verify session resets".
