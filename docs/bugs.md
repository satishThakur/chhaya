# Known bugs and accepted gaps

Open items we know about, with enough detail to act on without re-deriving them.
Each says what breaks, why it was not fixed at the time, and what it costs if left.

Fixed items move to the bottom under **Closed**, with the commit. Nothing is deleted —
a bug we decided not to fix is a decision, and the reasoning is worth keeping.

Severity here means: **blocker** stops a milestone, **degrades** weakens a safety
property without breaking it, **cosmetic** is wrong but harmless.

---

## B1 — `state.flush` uses a fixed temp path, so concurrent writers collide

**Severity:** degrades · **Found:** 2026-10-24, whole-branch review stress test · **Area:** `internal/state/state.go`

`flush` writes to `s.path + ".tmp"` and renames. Two processes writing at once — the
daemon recording a call while `chhayactl stop` sets the kill switch — race on that one
filename. Measured under two concurrent OS-process writers: **roughly half of writes fail**
with `rename …state.json.tmp…: no such file or directory`.

**What saves it:** the failure is loud and fail-closed. `dispatch` refuses with
`state.unwritable`; `chhayactl` prints the error and exits 1. A stress run of 20,000
concurrent reads against two writers produced no corrupt state file and no torn read.
It never silently succeeds, which is what the original cross-process bug did.

**Why it matters anyway:** this is on the kill-switch path. An operator running
`chhayactl stop` against a busy daemon may have to retry, and in the one scenario that
path exists for — the Telegram poll loop is dead, so `/stop` cannot be delivered — being
told to retry is worse than it sounds.

**Fix:** give the temp file a unique suffix (pid, or `os.CreateTemp` in the same
directory) so writers cannot collide. One line plus the `CreateTemp` cleanup path.

---

## B2 — `Invoke`'s doc comment claims an exception that now exists

**Severity:** cosmetic · **Found:** 2026-10-24, final re-review · **Area:** `internal/mcpsrv/gateway.go`

The comment says *"It always runs, in order: policy.Decide → dispatch-or-refuse →
audit.Append. There is no branch that skips the first or the last."* The audit-unwritable
latch added a dozen lines below is exactly such a branch — correctly, since a gateway that
cannot log must not act. The in-body comment is honest; the contract above it is not.

This is the same claims-versus-code defect class the branch spent eleven review rounds on,
which is the only reason it is listed rather than shrugged off.

**Fix:** reword to state the one exception.

---

## B3 — A test comment points at a test that no longer exists

**Severity:** cosmetic · **Found:** 2026-10-24, final re-review · **Area:** `internal/mcpsrv/gateway_test.go`

`TestRegistryIsUnreachableExceptThroughInvoke`'s comment names
`TestMCPHandlerDoesNotCallHandlersDirectly` as its compensating check. That test was
retired in the final fix wave, subsumed by `TestNoPathToAHandlerAnywhereInTheModule`.

**Fix:** repoint the comment.

---

## B4 — The handler-invariant test is a tripwire, not a proof

**Severity:** degrades · **Found:** 2026-10-24, final re-review · **Area:** `internal/mcpsrv/gateway_test.go`

`TestNoPathToAHandlerAnywhereInTheModule` walks the whole module and flags a direct
`reg.Handle(...)` call outside the allowlist. Verified working: a planted
`tools.All(d)[2].Handle(ctx, args)` in `cmd/chhayad` is caught.

It does **not** catch `reg.Handle` passed as a *function value* — `run(tools.All(d)[2].Handle)`
is invisible to it. That is the shape production code itself uses
(`callHandler(ctx, reg.Handle, args)`), which is why the allowlist entry for
`mcpsrv/gateway.go:dispatch` currently matches zero call sites and is dead.

So the spec's claim — "there is no exported function that reaches a handler directly" —
is true of `mcpsrv` and weaker than stated for the module. The `internal/` boundary still
keeps any bypass inside this repo.

**Fix:** either track function-value references in the AST walk, or amend the test's
comment and the spec to claim only what it proves. The second is honest and cheap; the
first is what the property deserves.

---

## B5 — Approval-card escaping is applied per call site, not at the transport

**Severity:** degrades · **Found:** 2026-10-24, final re-review · **Area:** `internal/approval/bot.go`

`card()` now escapes every interpolated value with `html.EscapeString`, so an agent cannot
close the `<code>` tag and forge a line the human then approves. Three other `SendMessage`
call sites still interpolate unescaped values into `parse_mode: HTML` text: a state error
string, `call.Name` in the timeout message, and `st.TaintSource` in `/status`.

**None is agent-controlled today** — approvals are only reached for registered tools, and
the taint source is a registration name. There is no live injection.

**Why it is listed:** the invariant now depends on every future call site remembering to
escape, which is precisely what the original finding objected to.

**Fix:** escape at the transport (in `SendMessage`, or a `sendHTML` wrapper that escapes its
arguments) rather than at each call site, so a new caller is safe by default.

---

## B6 — Whether Hermes' web and browser toolsets are actually off is unverified

**Severity:** degrades · **Found:** 2026-10-24, while writing `hermes/config.yaml` · **Area:** `hermes/config.yaml`

The taint model depends on the gateway being the only door for untrusted *reads*: the
gateway cannot taint-track a page Hermes fetched on its own. `docs/design.md` states the
web, browser and fetch toolsets are disabled and calls it architectural.

Hermes' `toolsets` key is an **allowlist**, defaulting to `["hermes-cli"]`, which suggests
web and browser tools are excluded unless added. But I did not confirm what the
`hermes-cli` toolset actually contains — the registry is not in `tools/toolsets.py` and I
stopped looking rather than guess.

**Until this is confirmed, taint tracking may be blind to the likeliest injection vector**,
which the design itself says is worse than no taint tracking because it reads as protection.

**How to verify:** in the live smoke test, ask the agent directly to fetch a URL. If it can,
the toolset allowlist does not exclude what we assumed and the config needs an explicit
exclusion. This is step 7 of the smoke-test checklist in `docs/runbook.md`.

---

## B8 — Hermes cycles on its own 60 s startup deadline

**Severity:** blocker · **Found:** 2026-10-24, first live bring-up · **Area:** `deploy/docker-compose.yml`, the pinned image

Hermes reaches `☤ Hermes Gateway Starting...`, connects Telegram in polling mode, then
exits with `Error: Gateway starting: deadline` roughly 60 s later and is restarted by s6.
`DEFAULT_ENSURE_TIMEOUT = 60.0` in `hermes_cli/gateway_runtime.py` is the client's wait for
a cold daemon; it is a hardcoded constant, not a config key. The daemon does come up — it
logs `Gateway control socket listening at /data/gateway.sock` — but never reports ready
inside the budget, so `container_boot` gives up and the service is torn down.

**What is ruled out.** Not the config (every setting loads — Hermes discovers
`mcp_servers.chhaya` and applies the providers block). Not egress (Telegram 1.49 s,
Anthropic reachable from the container's netns). Not `/data` ownership (the volume is
`10000:10000` and writable; s6's `fix-attrs` chowns it). Not the client key or the CA, both
fixed in `b2f545f`. Not load (loadavg 0.69–1.24 at the deadline).

**What is not ruled out.** Whether the MCP connection to the gateway is still in the
startup critical path and still failing for a *new* reason after `b2f545f` — the
certificate errors are gone from the logs, but no successful MCP connection has been
observed either, and the gateway has logged no completed handshake from Hermes. Also
untested: whether the boot-time self-update (see B9) is what eats the budget.

**What would settle it:** read `/data/logs/gateway.log` and `/data/logs/container-boot.log`
across one full cycle and find what the daemon is waiting on between
`control socket listening` and the SIGTERM ~59 s later.

---

## B9 — The pinned image self-updates at boot

**Severity:** degrades · **Found:** 2026-10-24 · **Area:** the pinned Hermes image

The image runs an updater on startup: `Handing off to the new updater (package manager)`,
then installs Python and Node dependencies and rebuilds the TUI (which itself fails with
`EXDEV: cross-device link not permitted`). The project rule is that Hermes is pinned to a
release and upgraded deliberately after reading the changelog; a container that mutates
itself at boot means the digest pins what we *start*, not what we *run*.

It also plausibly costs most of the startup budget in B8.

**Fix:** find the flag or env var that disables the boot-time update and set it in
`docker-compose.yml`, or accept it and say so in `docs/decisions.md`. Not yet investigated.

---

## Closed

### B10 — Four bugs that stopped Hermes starting, reaching the gateway, or verifying it

**Severity:** blocker · **Found:** 2026-10-24, first live bring-up · **Fixed:** `b2f545f`

Each was hidden behind the one before it, and three are the same mistake in different
clothes — a fact about a container's uid held in one file and contradicted in another.

1. **The first-run gate never consults `providers:`.** `_has_any_provider_configured`
   checks a fixed set of provider env var names (`PROXY_API_KEY` is not one), `auth.json`,
   then `model.provider` / `model.base_url` / `model.api_key`. A scalar `model:` misses all
   of them, so Hermes exited 1 with "no API keys or providers found" while the providers
   block loaded perfectly. `model` is now the dict form naming the provider. Verified both
   ways against the pinned image: scalar -> gate False, dict -> gate True.

2. **The rendered config was unreadable by Hermes.** `render_config` chmod 600'd it; s6
   runs the agent as uid 10000 (`s6-setuidgid hermes`), not the image's root `User`. Hermes
   then runs on defaults *silently*, which is the dangerous part — sampling, elicitation,
   the toolsets allowlist and `skills.write_approval` all revert. Now 0644; the file holds
   no secret.

3. **Hermes could not read its own client key**, same uid reason. The symptom named
   nothing: the MCP handshake never completed, Hermes missed its startup deadline, s6
   SIGTERMed it, and the container crash-looped looking like a network fault.

4. **The CA was malformed** — generated with no `basicConstraints` and no `keyUsage`. Go's
   `crypto/tls` and curl accept it, which is why `make dev-status` passed from the day the
   certs were made, but Python refuses it with `CA cert does not include key usage
   extension`, and Hermes verifies in Python. **A CA that only some verifiers accept is a
   CA that works until the one client that matters tries to use it.**

Also reverted in the same change: making the config mount read-write so Hermes could
migrate it. s6's `fix-attrs` chowns everything under `HERMES_HOME`, and a bind mount
propagates that to the *host* file — the rendered config came back `10000:10000 0640`, the
operator could no longer read it, and re-rendering failed. The migration failure is a
warning, not a fault.

Full reasoning, including why `SSL_CERT_FILE` points at a bundle rather than at `ca.crt`,
is in `docs/decisions.md`, 2026-10-24.

**Note for anyone updating:** certificates must be regenerated, and the stack must be
**stopped** first — Docker recreates a bind-mount source as a *directory* if it is deleted
while a container holds it.

---

### B7 — The gateway could not read its own TLS key

**Severity:** blocker · **Found:** 2026-10-24, first real `make dev-up` · **Area:** `deploy/gen-certs.sh`, `scripts/dev.sh`
**Fixed:** `0fdc5f3`

`gen-certs.sh` ended with `chmod 600 ./*.key`, leaving `gateway.key` owned by the host
user (uid 1000) while `Dockerfile.gateway` runs the gateway as uid 10001. Bind mounts do
not remap ownership, so the container saw `1000:1000 0600` and `chhayad` died on
`load server keypair: permission denied` — crash-looped by `restart: unless-stopped`.

Reproduced as uid 10001 against the real mount (`READ DENIED`), fixed, and re-probed
(`READ OK`). `gen-certs.sh` now grants that uid read on the key and traverse on the certs
directory, reading the uid out of `Dockerfile.gateway` so the two cannot drift again, and
reads the ACL back to prove it applied. `make dev-doctor` checks the same property, because
`gen-certs.sh` refuses to re-run over existing certificates and so cannot reach any install
made before this change.

Full reasoning, including why an ACL rather than `chmod 0644` or `chgrp`, is in
`docs/decisions.md`, 2026-10-24.

