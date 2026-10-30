# Memory approval card — Design

Oct 30, 2026 · status: draft, awaiting approval

**Parent spec:** [`docs/design.md`](../../design.md) §Personal memory: "Proposals land as a diff I approve in Telegram."
**Decision log:** [`docs/decisions.md`](../../decisions.md). This spec reverses one recorded promise (§6) and adds an entry for it.

## 1. Why

I told the agent "I do not like okra". It called `memory.propose_update`, got a proposal id, and nothing else happened. Today a proposal is a JSON file in `memory/.proposals/` that nothing reads. There is no way to approve it, and the file is `0600` owned by the gateway's uid (10001), so I can't even read it on the host.

The design always meant for proposals to be approved in Telegram. This builds that path.

**Success:** I say "I do not like okra". The approver bot posts a card. I tap approve. `memory/taste/food.md` gains the line, and `git status` shows it. Hermes plays no part between the proposal and the write.

## 2. Goals, non-goals, anti-goals

**Goals**
- A Telegram card on the **approver** bot for every memory proposal, with approve and reject buttons.
- On approve, the gateway writes the entry into the document itself.
- Proposal files are readable by the operator on the host.
- Every proposal, tap and write has an audit record. Every written line traces back to its proposal.

**Non-goals**
- Git commits. The gateway has no git. I review and commit with `git status`/`git diff`.
- Editing or removing existing lines. Proposals only append entries. Edits go through a person with an editor.
- Batching, digests, or a review queue UI.
- Hermes skill or SOUL.md changes beyond what the schema change forces.

**Anti-goals: this fails even if it technically works when**
- Any path lets Hermes cause a document write without a human tap. That includes Hermes tapping, elicitation, or any MCP tool that applies.
- What I approve differs from what lands in the file.
- A tap while `/stop` is active writes anything.

## 3. The card

Posted by the approver bot (never Hermes' bot), in one message with the keyboard attached by edit, the same way `Bot.Request` does it:

```
approve memory?
taste/food: No okra.
why: User explicitly stated in chat: "I do not like Okra"
[✅ approve] [❌ reject]
```

- The second line is the literal `entry` that will be written. It is not a summary and not a diff.
- All interpolated values are HTML-escaped, then truncated as `card()` does. The rationale is agent-controlled text.
- Button `callback_data` uses the existing HMAC token on the message id (`callbackToken`), prefixed with a distinct verb so memory taps can't be confused with tool-approval taps. For example `mem-approve:<tok>` / `mem-reject:<tok>`. Builder's choice of exact prefix.

After a tap the card text is edited and the buttons are removed:
- approve → `✅ written to taste/food`
- reject → `❌ rejected`
- write failed → `⚠️ not written: <reason>`. The buttons stay so the tap can be retried.

This needs `editMessageText` (with an empty `reply_markup`) added to the `Telegram` interface.

## 4. Flow

### 4.1 Propose (tool call, via `Invoke` as today)

`memory.propose_update` schema changes from `{path, diff, rationale}` to `{path, entry, rationale}`:
- `entry`: one line, the literal text to add, e.g. `No okra.`. Validated: non-empty after trim; no newline or carriage return; ≤ 200 characters (runes, not bytes); must not contain `<!--` or `-->` (it would break the provenance marker). A leading `- ` is stripped, because the gateway adds the bullet.
- `path`: unchanged, checked by `memoryPath`.
- `rationale`: unchanged, required.

The tool description must say the entry is written verbatim after human approval. Tier stays 0 (`TierInternal`, `Write`). The kill switch, rate limit and taint still apply through `Invoke` exactly as now.

Handler:
1. Validate, then write `memory/.proposals/<id>.json` (`0644`) with `proposal_id, path, entry, rationale, proposed_at`.
2. Post the card through a notifier the handler receives in `tools.Deps`. It is an interface implemented by the approver bot. `internal/tools` must not import `internal/approval`. A nil notifier fails closed: the proposal is refused, not silently staged.
3. If the card can't be posted, delete the proposal file and return an error telling Hermes the approver bot is unreachable. A proposal nobody can see is worse than none, and keeping it would let a retry produce duplicates.
4. Return `{proposal_id, status: "sent for approval"}`. **The tool does not wait for the tap.** There is no 300 s timeout. The card stays live until tapped or until the gateway restarts.

Under taint, `Invoke` first asks for the existing tool-approval card (sync, 300 s), then the handler posts the memory card. Two taps on a tainted session is acceptable and intended.

### 4.2 Tap (approver bot, not a tool)

The apply path lives in the approver's tap handling. It is not registered as an MCP tool and is not reachable from `mcpsrv`. Hermes has no route to it.

On **approve**:
1. Token check as today. Look up the pending memory card by message id. Unknown → "already answered".
2. **Ask `policy.Decide`** with a synthetic request: `Tool: "memory.apply"`, `Known: true`, `Tier: TierInternal`, `Kind: Write`, `ArgsOK: true`, `ValidatorOK: true`, and the current `state.Snapshot(now)`. Do not read the kill switch directly, because every write rule lives in `internal/policy` (CLAUDE.md). `Deny` (kill switch, rate limit) → write nothing. Answer with the decision's reason, keep the card and buttons live so a tap after `/resume` works, and audit as deny with the decision's rule. `NeedApproval` (taint) counts as satisfied: this tap *is* the human approval. `internal/policy` itself does not change. *(Builder-check call.)*
3. Re-read the proposal file and re-validate `path` (via `memoryPath`) and `entry`. Never trust the in-memory copy alone, and never trust that the file was not edited.
4. Append to the document (see §5). Before appending, scan the document for `<!-- proposal <id>,`. If it's already there (a crash between append and move), skip the append and go to step 5, so a retry never writes a second line. *(Builder-check call.)* If it fails, edit the card to `⚠️ not written`, keep it pending, and audit with `Result.Status: "error"`.
5. Move the proposal to `.proposals/applied/<id>.json`, adding `decided_at` and `decision`.
6. Audit record: `Actor: "approver"`, `Tool: "memory.apply"`, `Kind: write`, `Tier: 0`, args `{proposal_id, path}`. The entry is redacted the same way `propose_update` redacts today. `Decision: "allow"`, `Rule: "memory.tap_approved"`, `Approval.Answer: "approved"`.
7. Edit the card to `✅ written to <path>`.

On **reject**: move the proposal to `.proposals/rejected/<id>.json` (with `decided_at`, `decision`). Audit with `Decision: "deny"`, `Rule: "memory.tap_rejected"`, `Answer: "denied_by_user"`. Edit the card to `❌ rejected`. The kill switch does not block a reject, because a reject writes no document.

### 4.3 Restart

The callback secret is regenerated per process, so old buttons answer "that card has expired". That is deliberate and stays. At startup, after `Preflight`, the gateway posts a fresh card for every pending proposal: every `*.json` directly in `.proposals/` that has an `entry` field.

**Legacy proposals** (the okra one) have `diff` and no `entry`. Their `diff` is prose (`Add to dislikes list: "- Dislikes okra."`) and can't be applied mechanically. At startup they move to `.proposals/legacy/` with a log line. They get no card. I re-ask the agent.

## 5. Writing the document

- Target: `memoryPath(root, path)`. If the file doesn't exist, create it with `# <Title>\n\n`. The title is the last path segment with the first letter capitalised. The parent directory must already exist (`memoryPath` requires it). Creating new directories is out of scope.
- Line written: `- <entry> <!-- proposal <id>, approved <YYYY-MM-DD> -->\n`, date in UTC. If the file doesn't end in a newline, write one first.
- **Append with `O_APPEND`, never write-temp-then-rename.** A rename replaces the inode, so the operator-owned file would become uid-10001-owned and lose its ACLs.
- Proposal files are created `0644` and the `applied/`, `rejected/`, `legacy/` subdirectories `0755`. The operator only needs to read these.
- New documents are created `0664`, not `0644`. A `0644` create sets the ACL mask to `r--`, which would mask the operator's default-ACL `rwx` and leave a gateway-created document read-only to the person who is meant to edit it. *(Builder-check call.)*

## 6. Permissions and the reversed promise

`scripts/dev.sh` `prepare_proposals` today grants uid 10001 write on `memory/.proposals/` **only**. Its comment says this makes the filesystem enforce "propose_update never edits a document". That promise is replaced by:

> **The gateway edits a Personal memory document only on a human tap on the approver bot, never from a tool call.**

Changes:
- `dev.sh` grants the gateway uid `rwX` on `memory/` recursively, **excluding `memory/.git` if present**. It adds default ACLs so files the gateway creates stay writable by the operator, and files the operator creates stay writable by the gateway. Update the preflight check and its `setfacl` hint to match.
- Rewrite the `dev.sh` comment to state the new guarantee and where it is enforced (the tap handler, plus the tests in §7).
- Append a `docs/decisions.md` entry: what changed, that the apply path calls `policy.Decide` with a synthetic `memory.apply` request and treats `NeedApproval` as satisfied by the tap, why (Hermes is out of the loop between proposal and write; the human tap is the gate), and what would reopen it. For example, any second writer to `memory/` inside the gateway.
- Update `hermes/SOUL.md`: `memory_propose_update` now takes `entry`, one literal line such as `No okra.`, and the result means "sent to the approver bot", not "recorded". The agent tells the user to approve on the approver bot.
- Update `docs/runbook.md` step 4 and step 9: the expected result is now a card on the approver bot, and the file changes only after approve.

## 7. Tests that must exist

Failing test first for each, as CLAUDE.md requires for the approval path.

Blocked cases stay blocked:
- Tap with a forged or other-message token → no write, no file move.
- Tap from a user id other than the approver → no write, critical audit (existing path, now covering memory taps).
- Second tap on an answered card → "already answered", no second line.
- Approve while the kill switch is on → no write; the card stays tappable; approve after `/resume` writes exactly one line.
- Proposal file edited on disk between propose and tap so `path` escapes the root (`../x`, absolute path, symlink out) → no write.
- Entry containing a newline, `-->`, over 200 chars, or empty → rejected at propose; also rejected at apply if the file was edited to contain it.
- No registered MCP tool (walk the registry) can apply a proposal. Extend the existing module-walk invariant test so the apply function is reachable only from the approver's tap handler and tests.
- Card-post failure → proposal file removed, tool returns an error.

Happy paths:
- Approve appends exactly `- No okra. <!-- proposal …, approved … -->` to an existing file without a trailing newline, and preserves the file's inode (prove no rename).
- Approve on a new path creates the titled document.
- Reject moves to `rejected/` and leaves the document byte-identical.
- Startup re-posts pending proposals and moves legacy ones.
- Proposal files and subdirectories have the stated modes. A gateway-created document is `0664`.
- An approve retried after the line was already appended (proposal still pending) writes no second line.
- A tainted state does not block an approve tap; a kill-switch state does.
- Card text escapes HTML in entry and rationale. The entry line is never truncated (its 200-rune limit is under the card's truncation).

`make test` and `make evals` pass.

## 8. Left to the builder

- Package layout. Suggested: a small `internal/memory` package owning the proposal file format, validation, list/move and append. Both `internal/tools` and `internal/approval` use it, so neither imports the other.
- Exact callback verb prefixes, card wording beyond §3, the notifier interface shape.
- How pending memory cards are tracked in the bot (a second map, or a tagged entry in `waiting`).
- Whether `chhayactl` gets a `memory pending` listing. Optional, not required.
