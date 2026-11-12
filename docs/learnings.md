# Our learnings

What went wrong or surprised us, what we found the cause to be, and what we changed. Not the
same as `docs/decisions.md`, which records choices and what would reopen them. A learning is
evidence; a decision is what we did about it. Add to the bottom, dated, with the evidence.

Each entry: **What we saw**, **Cause**, **What we did**, **Still open**.

---

## 1. The agent "forgot" because we gave it no way to look (2026-11-08)

**What we saw.** The agent kept asking for facts it had already been told, and "find coffee near
my home" failed at first. It looked like forgetting.

**Cause.** The gateway had only `memory.get` and `memory.propose_update`. `get` needs an exact
path, so with no way to list what existed the agent guessed (`location`, `addresses`, `home`,
`profile`) and mostly missed. Session history in `state.db` showed it making zero memory calls
and asking for the address, which was saved under `location`. There were two causes, not one:
discovery was missing, and the agent did not look before asking.

**What we did.** Added `memory.list` (documents, titles and entry counts; capped, no hidden state,
no symlinks) and changed the agent's instructions in `hermes/SOUL.md` to call `memory_list` then
`memory_get` before asking the user for a fact about themselves. Confirmed in a live session:
"find a movie theatre near my home" went list, get, answer.

**Still open.** One session is not a measurement. The eval should score "did it look" and "did it
find" separately. `memory.search` is in the design and still not built.

**Lesson.** A tool set is the agent's whole world. If it can read but not discover, it will guess
or ask, and from outside that looks like a memory problem. Check what the agent can do before
deciding what it knows.

## 2. Agent writes went to the wrong place (2026-11-07)

**What we saw.** File writes by the agent were denied inside its home, or landed somewhere that
vanished when the container was recreated.

**Cause.** The image defaults `HERMES_WRITE_SAFE_ROOT` to `/opt/data`. Our home is `/data`, so the
default pointed at a directory Hermes does not use.

**What we did.** Set it to `/data/workspace`. Deliberately not `/data`: that would put `skills/`,
`.env`, `memories/` and `cron/` in reach of `write_file` and could sidestep `skills.write_approval`.

**Still open.** Not verified whether the `terminal` tool honours the same root, or whether a skill
written through `write_file` would bypass approval.

**Lesson.** An image default written for the image's own layout is not safe to inherit. Read what
a path setting points at in your layout.

## 3. Personal data does not belong in a public repo (2026-11-08)

**What we saw.** Personal memory (addresses, tastes) was committed in the code repo, which is public.

**Cause.** Memory started life as files beside the code because that was the simplest place.

**What we did.** Moved it to `~/.chhaya/gateway/memory`, its own local git repo with no remote,
gitignored the old path, and kept a generic `memory.example/` as the seed. Added a non-negotiable
to `CLAUDE.md`.

**Lesson.** Decide early whether something is code or data. Data gets its own location, its own
backup, and never a path into the public repo.

## 4. State that must outlive Docker cannot live in Docker volumes (2026-11-07)

**What we saw.** Hermes' memory, sessions and cron jobs sat in named volumes, which `down -v` or a
prune can delete. These were real conversations.

**What we did.** Moved Hermes' home and the gateway's data to host bind-mounted directories under
`~/.chhaya`, with backups taken before and counts compared after. Hermes' own Docker docs use bind
mounts too.

**Lesson.** Before moving data, back it up and count what is in it; after, compare. The old
volumes were kept until the new layout was proven.

## 5. Two machines on one Telegram bot would have broken both (2026-11-08)

**What we saw.** Once production moved to EC2, the desktop and the box would have shared the same
two bots.

**Cause.** One bot token allows one poller. Two pollers fight over `getUpdates` and drop messages,
and an approval tap can land on the wrong machine. We caught this by thinking it through, not by
seeing it fail.

**What we did.** Separate bot pairs per environment, and a guard: `CHHAYA_ENV=dev|prod` in both
env files, and preflight calls Telegram `getMe` on each token and refuses on any mismatch, swap,
shared bot or unreachable Telegram. Tests cover each refusal.

**Lesson.** When a mistake would be silent and expensive, make it impossible to start the stack
that way, not merely documented.

## 6. A constant outlived the reason for it (2026-11-09)

**What we saw.** `make dev-certs` failed on the fresh box with `setfacl: command not found`.

**Cause.** `gen-certs.sh` and the preflight still granted read access to uids 10001 and 10000
through ACLs. That was needed while the containers ran as those uids. After the bind-mount move
both run as the operator's uid, so a `0600` key the operator owns was already enough. It had only
worked on the desktop because the `acl` package happened to be installed.

**What we did.** Removed the ACL grants and checked key readability against the operator's uid.

**Lesson.** Changing how something runs means searching for what that change made obsolete. A
first run on a clean machine finds what a long-lived one hides.

## 7. A read-only mount can be right even when it logs an error (2026-11-07)

**What we saw.** Hermes logs `[config-migrate] ERROR: Read-only file system: '/data/config.yaml'`
on every boot.

**Cause and choice.** Hermes tries to migrate its own config in place. We mount the rendered config
read-only. Tried read-write: the image's startup chowns everything under its home, which on a bind
mount changes the owner of the file on the host, and the operator could no longer read it. The
migration failing is only a warning; every setting in the file is still applied.

**What we did.** Kept it read-only, so `hermes/config.yaml` stays the single source of truth, and
documented the expected error in `docker-compose.yml` and `docs/ship-a-change.md`.

**Lesson.** Not every ERROR line is a fault. Before "fixing" a recurring error, find out what it
protects and what silencing it would cost.

## 8. `make dev-up` does not restart Hermes when only its config changed (2026-11-12)

**What we saw.** After adding the session-reset block to `hermes/config.yaml` and running `make dev-up`,
the rendered file had the new block but `hermes plugins list` showed the plugin disabled and nothing reset.

**Cause.** The rendered config is a single bind-mounted file. Rewriting it leaves the container definition
unchanged, so Compose does not recreate the container, and Hermes reads its config only at start.

**What we did.** Restart Hermes after a config-only change: `docker restart deploy-hermes-1`. Recorded in
`docs/session-reset.md`. A change to an env file is different: it needs a recreate (`make dev-up`), as the
request-dump flag did.

**Still open.** `dev-up` could say when it rendered a changed config and Hermes was not restarted.

## 9. A reset session recovered its context by searching (2026-11-12)

**What we saw.** After the idle reset, asked "what was my last message?", the agent answered correctly from
a session with no history.

**Cause.** Hermes has a `session_search` tool over `state.db`. The agent found it with `tool_search`, browsed
the previous session and read it. The reset closes a session; it does not delete it, and each new session
records its predecessor as its parent.

**What we did.** Nothing; it removes much of the worry that a 30-minute reset loses context. Not shown: whether
it searches without being asked, which matters for replies to the digest.

**Still open.** Watch for conversations that open with an implicit reference during annotation.
