# Memory Approval Card Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A memory proposal posts an approve/reject card on the approver bot, and an approve tap makes the gateway append the entry to the Personal memory document.

**Architecture:** A new pure-filesystem package `internal/memory` owns the proposal file format, validation, path containment and the append. `internal/tools` stages a proposal and calls a notifier. `internal/approval` implements the notifier, renders the card, and on a tap asks `policy.Decide` before calling `memory.Store.ApplyApproved`. `internal/tools` and `internal/approval` never import each other.

**Tech Stack:** Go 1.24, standard library only, `go test`. Bash for `scripts/dev.sh`.

**Spec:** `docs/superpowers/specs/2026-10-30-memory-approval-card-design.md`

## Global Constraints

- Work on branch `feat/memory-approval-card` in worktree `/home/satishthakur/codebase/chhaya-memory-approval`. Never touch `/home/satishthakur/codebase/chhaya`, where the user's stack is running.
- Do not edit anything under `memory/` in the repo. Tests use `t.TempDir()`.
- `entry`: trimmed, a leading `- ` stripped, non-empty, no `\n` or `\r`, ≤ 200 **runes**, must not contain `<!--` or `-->`.
- Written line: `- <entry> <!-- proposal <id>, approved <YYYY-MM-DD UTC> -->\n`.
- Append with `os.O_APPEND`, never temp-file-and-rename.
- Modes: proposal files `0644`; `.proposals/{applied,rejected,legacy}/` `0755`; a document the gateway creates `0664`.
- A new document starts `# <Title>\n\n`, where Title is the last path segment with its first letter upper-cased.
- Card text is HTML. Escape every interpolated value with `html.EscapeString`. Truncate before escaping, never after.
- Audit for a tap: `Actor "approver"`, `Tool "memory.apply"`, `Kind "write"`, `Tier 0`, args `{proposal_id, path}` (never the entry). Approve uses `Decision "allow"`, `Rule "memory.tap_approved"`, `Answer "approved"`. Reject uses `Decision "deny"`, `Rule "memory.tap_rejected"`, `Answer "denied_by_user"`.
- Failing test first for every behaviour. `make test` and `make evals` must pass at the end of every task. Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A document with no trailing newline, or an empty file.** The entry must land on its own line, not glued to the last line. Pinned in Task 2.
2. **An entry containing `<`, `&` or `"` (e.g. `No "masala" & no <b>okra</b>`).** It must be written to the file byte-for-byte and escaped on the card. Pinned in Tasks 2 and 3.
3. **A non-ASCII entry (e.g. 200 × `भ`).** It must be accepted, because the limit is 200 runes, not 200 bytes. Pinned in Task 1.
4. **A proposal file removed by hand before the tap.** The tap must write nothing, edit the card to `⚠️ not written: …`, and not panic. Pinned in Task 3.
5. **Two proposals for the same document approved one after the other.** Both lines must appear, in tap order. Pinned in Task 2.

---

### Task 1: `internal/memory` — proposals, validation, containment

**Files:**
- Create: `internal/memory/memory.go`, `internal/memory/memory_test.go`
- Modify: `internal/tools/tools.go`. Delete `memoryPath` and `containedIn` and call `memory.Resolve` instead. Behaviour is unchanged; the existing `TestMemoryGet*` and out-of-root tests must stay green unedited.
- Task 4 deletes `newID` from `tools.go`, once `Stage` is the only caller.

**Interfaces:**
- Produces:
  - `func Resolve(root, rel string) (string, error)`. This is today's `memoryPath`, moved verbatim with its comments.
  - `func ValidateEntry(s string) (string, error)`. Returns the normalised entry.
  - `type Proposal struct { ID string \`json:"proposal_id"\`; Path string \`json:"path"\`; Entry string \`json:"entry,omitempty"\`; Diff string \`json:"diff,omitempty"\`; Rationale string \`json:"rationale"\`; ProposedAt string \`json:"proposed_at"\`; DecidedAt string \`json:"decided_at,omitempty"\`; Decision string \`json:"decision,omitempty"\` }`. `Diff` exists only to recognise legacy files.
  - `type Store struct{ Root string }`
  - `func (s Store) Stage(path, entry, rationale string, now time.Time) (Proposal, error)`. Validates, resolves, writes `.proposals/<id>.json` with mode `0644`. The ID format is today's `newID()`, moved here.
  - `func (s Store) Load(id string) (Proposal, error)`. Refuses any id not matching `^\d{8}-[0-9a-f]{16}$` before touching the filesystem.
  - `func (s Store) Remove(id string) error`
  - `func (s Store) Pending() (ready []Proposal, legacy []string, err error)`. Covers `*.json` directly in `.proposals/`. A file with an `entry` is ready; one without is legacy. Sorted by ID.
  - `func (s Store) MoveLegacy(id string) error`. Moves the file to `.proposals/legacy/`.

- [ ] **Step 1: Write the failing tests** in `internal/memory/memory_test.go`:
  - `TestValidateEntry`, table-driven:
    - `"No okra."` → `"No okra."`
    - `"- No okra."` → `"No okra."`
    - `"  No okra.  "` → `"No okra."`
    - `strings.Repeat("भ", 200)` → ok
    - each of these → error: `strings.Repeat("a", 201)`, `""`, `"   "`, `"a\nb"`, `"a\rb"`, `"x <!-- y"`, `"x --> y"`
  - `TestStageWritesAReadableProposal`: `Stage("taste/food", "No okra.", "said so", now)`. Assert the file exists with mode `0644`, round-trips through `Load`, and that `taste/food.md` is unchanged.
  - `TestStageRefusesBadInput`: each of the following errors and leaves no file in `.proposals/`: path `../x`, path `/etc/passwd`, an entry with a newline, an empty rationale.
  - `TestLoadRefusesAnIDThatIsAPath`: `Load("../../etc/passwd")` and `Load("20261030-zz")` both error.
  - `TestPendingSplitsLegacyFromReady`. Write one staged proposal and one raw legacy file `{"proposal_id":"20261029-688f393323e57be8","path":"taste/food","diff":"Add to dislikes list","rationale":"x","proposed_at":"…"}`. Assert `ready` has 1 and `legacy == ["20261029-688f393323e57be8"]`. Then `MoveLegacy` it, and assert the file is in `legacy/`, the directory mode is `0755`, and `Pending` now returns no legacy.

- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/memory/...`. Expected: build failure, package or functions undefined.
- [ ] **Step 3: Implement** `internal/memory/memory.go` to the interfaces above. Move `memoryPath` and `containedIn` out of `tools.go`, and copy `newID` into `memory` as unexported. In `tools.go`, `memory.get` and `propose_update` call `memory.Resolve`. Otherwise leave `propose_update` and its own `newID` alone; Task 4 removes them.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `go test ./internal/memory/... ./internal/tools/...`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(memory): proposal store, entry validation, containment moved out of tools"`

### Task 2: `ApplyApproved` and `Reject`

**Files:**
- Modify: `internal/memory/memory.go`, `internal/memory/memory_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `func (s Store) ApplyApproved(id string, now time.Time) (Proposal, error)`
  - `func (s Store) Reject(id string, now time.Time) (Proposal, error)`
  - Both return the proposal as moved, with `DecidedAt` (RFC3339 UTC) and `Decision` (`"approved"` or `"rejected"`).

`ApplyApproved` does the following, in order:
1. `Load`.
2. Re-run `ValidateEntry` and `Resolve` on the loaded values. Never trust the file.
3. If the document is missing, create it with `O_CREATE|O_EXCL`, mode `0664`, holding the title header.
4. If the document already contains `<!-- proposal <id>,`, skip the append.
5. Otherwise open with `O_APPEND|O_WRONLY`. If the file is non-empty and does not end in `\n`, write `\n` first. Then write the line.
6. Move the proposal JSON to `.proposals/applied/<id>.json` with the decision fields set and mode `0644`, and remove the original.

`Reject` does only step 6, into `rejected/`, and never opens the document.

- [ ] **Step 1: Write the failing tests:**
  - `TestApplyAppendsExactlyOneLineAndKeepsTheInode`. The doc is `"# Food\n\nNo coriander."`, with no trailing newline. Record its inode via `os.Stat(...).Sys().(*syscall.Stat_t).Ino`. Apply at `2026-10-30T10:00:00Z`. Expect the file to equal `"# Food\n\nNo coriander.\n- No okra. <!-- proposal <id>, approved 2026-10-30 -->\n"`, the inode to be unchanged, and the proposal to be in `applied/` and gone from `.proposals/`.
  - `TestApplyOnAnEmptyDocument`: the doc is `""`. Expect exactly the one line, with no leading newline.
  - `TestApplyWritesSpecialCharactersVerbatim`: entry `No "masala" & no <b>okra</b>`. Expect those bytes in the file.
  - `TestApplyCreatesANewTitledDocument`: path `taste/drinks`. Expect the file to be `"# Drinks\n\n- … -->\n"` with mode `0664`.
  - `TestApplyIsIdempotentAfterACrash`. Append the line by hand to simulate a crash after the append and before the move, leaving the proposal pending. `ApplyApproved` must leave exactly one line containing the id and move the proposal.
  - `TestTwoApprovalsLandInOrder`: stage A then B for `taste/food`, apply B then A. Expect B's line before A's line.
  - `TestApplyRefusesAProposalEditedToEscape`. Stage, then rewrite the JSON so `path` is `../outside`. Do the same with `entry` set to `"x\ny"`, and with `taste` replaced by a symlink to a directory outside the root. Each must return an error, leave nothing written outside the root, and leave the proposal in place.
  - `TestApplyOnAMissingProposalErrors`: `ApplyApproved` on a staged-then-`Remove`d id errors and writes nothing.
  - `TestRejectLeavesTheDocumentByteIdentical`: compare bytes before and after, and expect the proposal in `rejected/` with `Decision == "rejected"`.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/memory/...`. Expected: FAIL, methods undefined.
- [ ] **Step 3: Implement** `ApplyApproved` and `Reject` as described above.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `go test ./internal/memory/...`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(memory): apply an approved proposal by O_APPEND, reject without touching the document"`

### Task 3: The approver's memory card and tap handling

**Files:**
- Create: `internal/approval/memorycard.go`, `internal/approval/memorycard_test.go`
- Modify: `internal/approval/telegram.go` (add `EditMessage`), `internal/approval/bot.go` (route memory verbs in `tap`, new `NewBot` parameters), `internal/approval/bot_test.go` (fake gets `EditMessage` and `newTestBot` is updated), `cmd/chhayad/main.go` (the `NewBot` call only; real wiring is Task 5)

**Interfaces:**
- Consumes: `memory.Store`, `memory.Proposal`, `ApplyApproved`, `Reject`, `Pending`, `MoveLegacy` from Tasks 1–2. Also `policy.Decide(policy.Request, policy.State, policy.Config, time.Time) policy.Decision`.
- Produces:
  - In `Telegram`: `EditMessage(ctx context.Context, chatID, messageID int64, text string) error`. This is `editMessageText` with `parse_mode: HTML` and `reply_markup: {"inline_keyboard": []}`, so the buttons go away.
  - `func NewBot(tg Telegram, chatID int64, st *state.Store, w *audit.Writer, timeout time.Duration, cfg policy.Config, mem memory.Store) *Bot`
  - `func (b *Bot) ProposeMemory(ctx context.Context, p memory.Proposal) error`. It sends the card, registers `b.memWaiting[msgID] = p.ID` (a new `map[int64]string` under `b.mu`) **before** attaching the keyboard, then attaches `[✅ approve | mem-approve:<tok>] [❌ reject | mem-reject:<tok>]`. If the attach fails, it deletes the map entry and returns the error.
  - `func (b *Bot) RepostPending(ctx context.Context) error`. It calls `MoveLegacy` and logs each legacy id. For each ready proposal it calls `ProposeMemory`. It returns the first error but keeps going through the rest.

Card text, exactly:
```
<b>approve memory?</b>
<code>{path}</code>: {entry}
why: {truncate(rationale, 300)}
```
Edited texts:
- approve: `✅ written to <code>{path}</code>: {entry}`
- reject: `❌ rejected: <code>{path}</code>: {entry}`
- failure: `⚠️ not written: {reason}` (reason truncated to 200)

All interpolated values are escaped.

Routing in `tap`: the HMAC token check stays first and is shared. Then `mem-approve` and `mem-reject` go to `b.memoryTap(ctx, c, approve bool)`. Every other verb keeps today's path, unchanged.

`memoryTap`:
1. Claim the id by deleting it from `memWaiting` under the lock. If it's absent, answer "already answered" and stop.
2. **Reject:** call `Reject`. Then audit, answer the callback, and edit the card. If `Reject` errors, re-insert the id and edit the card to `⚠️`.
3. **Approve:**
   - Call `policy.Decide(policy.Request{Tool: "memory.apply", Known: true, Tier: policy.TierInternal, Kind: policy.Write, ArgsOK: true, ValidatorOK: true}, b.state.Snapshot(now), b.cfg, now)`.
   - On `Deny`: re-insert the id, answer with `d.Reason`, audit a deny with `d.RuleID`. Leave the card text and buttons as they are.
   - Otherwise (`Allow`, or `NeedApproval`, which the tap satisfies): call `b.state.RecordCall("memory.apply", now)`, then `ApplyApproved`.
   - If `ApplyApproved` errors: re-insert the id, edit the card to `⚠️`, audit with `Result{Status: "error", Error: err.Error()}`.
   - On success: audit, answer "written", edit the card to `✅`.

- [ ] **Step 1: Write the failing tests** in `memorycard_test.go`. Extend `fakeTG` with an `edits map[int64]string` and its `EditMessage`. Add a helper `memTap(msgID int64, verb string) Update` that builds a callback from `allowedUser` with the correct token.
  - `TestProposeMemoryPostsTheCard`: the sent text contains `approve memory?`, `<code>taste/food</code>: No okra.` and `why: said so`.
  - `TestTheMemoryCardEscapesAgentText`: entry `No "masala" & no <b>okra</b>`, rationale `</code><b>forged</b>`. Reuse `tagSequence` from `bot_test.go` to assert the only tags present are the card's own `b` and `code`.
  - `TestApproveTapWritesAndEditsTheCard`: after `b.handle(ctx, memTap(id, "mem-approve"))`, the document has the line, the edit text starts with `✅ written to`, and the last audit record has `Tool "memory.apply"`, `Decision "allow"`, `Rule "memory.tap_approved"`, `Approval.Answer "approved"`, and args without `entry`.
  - `TestRejectTapWritesNothing`: the document is byte-identical, the edit starts with `❌ rejected`, and the audit has `Rule "memory.tap_rejected"`.
  - `TestAForgedMemoryTapWritesNothing`: a token minted for a different message id → document unchanged, proposal still pending.
  - `TestAMemoryTapFromAnotherUserWritesNothing`: `From.ID = allowedUser+1` → document unchanged, and a critical `approver.unauthorized_sender` record.
  - `TestASecondTapIsAlreadyAnswered`: approve twice → one line, and the second answer is "already answered".
  - `TestApproveWhileStoppedWritesNothingThenWorksAfterResume`: `SetKillSwitch(true)`, approve → document unchanged, an audit deny with `Rule "killswitch.active"`, no edit. `SetKillSwitch(false)`, approve again → exactly one line.
  - `TestTaintDoesNotBlockAnApproveTap`: `SetTaint(true, "web.fetch", now)`, approve → line written.
  - `TestAMissingProposalFileEditsTheCardToNotWritten`: `Remove` the proposal file before the tap → the edit starts with `⚠️ not written`, no document change, no panic.
  - `TestRepostPendingPostsReadyAndShelvesLegacy`: one ready and one legacy file → exactly one card sent, and the legacy file is in `legacy/`.
  - `TestOnlyTheTapHandlerApplies`. Walk the module the way `TestNoPathToAHandlerAnywhereInTheModule` does (copy `moduleRoot` and the walk skeleton; do not import test code across packages). Every call expression whose selector is `ApplyApproved` must be inside `internal/approval/memorycard.go` func `memoryTap`, or in `internal/memory/memory_test.go`. Assert at least 20 files were parsed.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/approval/...`. Expected: build failure on `EditMessage`, `ProposeMemory` and the `NewBot` arity.
- [ ] **Step 3: Implement** `EditMessage` (http and fake), the `NewBot` signature (update `newTestBot` to pass `policy.Config{DefaultRatePerHour: 60}` and `memory.Store{Root: t.TempDir()}`, and the `main.go` call to pass `cfg` and `memory.Store{Root: memoryRoot}`), and `memorycard.go`.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `go test ./internal/approval/... ./cmd/... && make evals`. Expected: PASS. The existing bot tests are unchanged apart from the `newTestBot` call.
- [ ] **Step 5: Commit.** `git commit -m "feat(approval): memory approval card; a tap asks policy.Decide, then applies"`

### Task 4: `memory.propose_update` takes `entry` and posts the card

**Files:**
- Modify: `internal/tools/tools.go`, `internal/tools/tools_test.go`, `test/e2e/e2e_test.go` (args only)

**Interfaces:**
- Consumes: `memory.Store.Stage`, `memory.Store.Remove`, `memory.Proposal`.
- Produces:
  - In `internal/tools`: `type MemoryNotifier interface { ProposeMemory(ctx context.Context, p memory.Proposal) error }`.
  - A new `Deps` field: `Notifier MemoryNotifier`.
  - `*approval.Bot` satisfies this structurally. Do not import `approval`.

Registration changes:
- Schema: `{path, entry, rationale}`, all three required. `entry` description: `"one line, written verbatim to the document after the user approves it, e.g. \"No okra.\""`.
- `Redact: []string{"entry"}`. Tier, kind and trust are unchanged.
- Description: `"Propose one line for a Personal memory document. This does NOT write the document: it sends the line to the user's approver bot, and the gateway appends it only if they tap approve. A rationale is required — it is recorded as provenance."`

Handler order:
1. If `d.Notifier == nil`, return the error `"memory proposals are disabled: no approver is wired"` before staging anything.
2. Call `Stage`.
3. Call `ProposeMemory`. If it errors, call `Remove(id)` and return `"the approver bot did not receive the proposal, so nothing was saved: <err>. Tell the user their approver bot is not responding."`.
4. Return `{"proposal_id": id, "status": "sent for approval"}`.

- [ ] **Step 1: Update and write the failing tests.** Add a `fakeNotifier{got []memory.Proposal; err error}` and set it in `deps()`.
  - Rewrite `TestProposeUpdateWritesAProposalNotTheFile` to use `"entry": "No mushrooms either."`. Additionally assert that the notifier received the proposal and that `status == "sent for approval"`.
  - Update `TestProposeUpdateRequiresARationale` and `TestProposeUpdateRefusesOutOfRootPath` to send `entry`.
  - `TestProposeUpdateRefusesAMultiLineEntry`: `"a\nb"` → error, and nothing in `.proposals/`.
  - `TestProposeUpdateWithAFailingNotifierSavesNothing`: notifier error → handler error, and `.proposals/` has no `*.json`.
  - `TestProposeUpdateWithNoNotifierFailsClosed`: `Notifier: nil` → error, and nothing staged.
  - Keep `TestTheRegisteredSetIsExactlyWhatWeExpect` unchanged. It must still pass.
- [ ] **Step 2: Run them and confirm they fail.** Run `go test ./internal/tools/...`. Expected: FAIL. The current handler requires `diff` and never calls a notifier.
- [ ] **Step 3: Implement** the handler as described above. In `test/e2e/e2e_test.go`, change both `"diff": "+ nothing"` args to `"entry": "nothing"`. Those calls are refused by the kill switch before the handler runs, so a nil `Notifier` is correct there.
- [ ] **Step 4: Run the tests and confirm they pass.** Run `make test && make evals && make e2e`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(tools): propose_update takes one literal entry and sends it to the approver"`

### Task 5: Wire the daemon, permissions, and docs

**Files:**
- Modify: `cmd/chhayad/main.go`, `scripts/dev.sh`, `hermes/SOUL.md`, `docs/runbook.md`, `docs/decisions.md`

**Interfaces:**
- Consumes: `bot.RepostPending`, `tools.Deps.Notifier`.

Changes:
- `main.go`: set `Notifier: bot` in `tools.Deps`. After `Preflight` and the startup card, and before `go bot.Run(ctx)`, call `bot.RepostPending(ctx)`. A failure is **logged, not fatal**: the proposals stay on disk and are re-posted on the next start.
- `dev.sh`:
  - Rename `prepare_proposals` to `prepare_memory`. It takes `guid` and `me=$(id -u)`, then:
    - `mkdir -p memory/.proposals`
    - For every path under `memory/` except `memory/.git`, use `find "$ROOT/memory" -path "$ROOT/memory/.git" -prune -o -print0 | xargs -0 setfacl -m "u:${guid}:rwX"`.
    - On directories only, add the default ACLs `setfacl -d -m "u:${guid}:rwX,u:${me}:rwX"`.
  - The verify step uses `writable_dir_by memory/taste "$guid"` and `writable_dir_by memory/.proposals "$guid"`. The preflight check and its `red` hint name the same command.
  - Rewrite the comment to state the new guarantee, *"the gateway edits a Personal memory document only on a human tap on the approver bot, never from a tool call"*, and point to `docs/decisions.md` 2026-10-30.
- `SOUL.md`, the "To remember something" bullet:
  - `memory_propose_update` takes `path`, `entry` (one literal line, e.g. `No okra.`) and `rationale`.
  - It sends the line to the user's approver bot. Nothing changes until they tap approve there.
  - Tell the user to approve it on the approver bot.
- `runbook.md`:
  - Step 4 expects a card on the **approver** bot, and `food.md` unchanged until the tap.
  - Add a step 4b: tap approve, and the line appears in `memory/taste/food.md` with the provenance comment.
  - Step 9 expects a card.
- `decisions.md`: append `## 2026-10-30 — The gateway writes Personal memory, but only on a human tap`. It records:
  - the old promise (filesystem-only grant on `.proposals`) and why it changed (Hermes is out of the loop between proposal and write; the tap is the gate);
  - that the apply path calls `policy.Decide` with a synthetic `memory.apply` request and treats `NeedApproval` as satisfied by the tap;
  - O_APPEND not rename, and why;
  - `0664` and why;
  - legacy `diff` proposals are shelved, not converted;
  - what reopens it: any second writer to `memory/` inside the gateway, or any MCP tool that reaches `ApplyApproved`.

- [ ] **Step 1: Wire `main.go`** as described.
- [ ] **Step 2: Run** `go build ./... && make test && make evals && make e2e`. Expected: PASS.
- [ ] **Step 3: Edit `dev.sh`** and run `bash -n scripts/dev.sh`. Expected: no output. Run `scripts/dev.sh doctor` from the worktree; it must not modify anything. Do **not** run `up`, `restart` or `down`: the user's stack is running.
- [ ] **Step 4: Edit `SOUL.md`, `runbook.md` and `decisions.md`.**
- [ ] **Step 5: Commit.** `git commit -m "feat: wire the memory card into chhayad; grant the gateway memory/ on a tap-only basis"`

### Task 6: Final verification

- [ ] **Step 1:** In the worktree, run `make test && make evals && make e2e && go vet ./...`. Expected: all PASS, vet silent.
- [ ] **Step 2:** Run `git diff main --stat` and confirm no file under `memory/` changed.
- [ ] **Step 3:** Report to the user. To try it live, merge or check out the branch, `make dev-up` (re-runs the ACL grant), then tell the agent "I do not like okra". The okra proposal from 2026-10-29 will be moved to `memory/.proposals/legacy/` at startup.
