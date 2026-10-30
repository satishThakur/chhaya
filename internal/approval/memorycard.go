package approval

import (
	"context"
	"fmt"
	"html"
	"os"
	"time"
	"unicode/utf8"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/policy"
)

// memoryTool is the name a memory tap is decided and audited under. It is not
// a registered tool: nothing the agent can call reaches ApplyApproved.
const memoryTool = "memory.apply"

// ProposeMemory posts the card for a staged proposal. Unlike Request it does
// not wait: the proposal sits on disk, and the tap is handled whenever it comes.
func (b *Bot) ProposeMemory(ctx context.Context, p memory.Proposal) error {
	// Every value on the card is agent text, and the card is HTML. Truncate
	// first, then escape, so a cut never lands inside an entity.
	text := fmt.Sprintf("<b>approve memory?</b>\n<code>%s</code>: %s\nwhy: %s",
		html.EscapeString(p.Path), html.EscapeString(p.Entry), html.EscapeString(truncate(p.Rationale, 300)))
	msgID, err := b.tg.SendMessage(ctx, b.chatID, text, nil)
	if err != nil {
		return fmt.Errorf("could not send the memory card: %w", err)
	}
	// Registered before the keyboard goes on, for the same reason as in
	// Request: attaching it is the earliest moment a tap can arrive. The
	// whole proposal is kept, not just its id, because it is the record of
	// what the human was shown.
	b.mu.Lock()
	b.memWaiting[msgID] = p
	b.mu.Unlock()

	if err := b.tg.AttachKeyboard(ctx, b.chatID, msgID, memoryButtons(msgID)); err != nil {
		b.mu.Lock()
		delete(b.memWaiting, msgID)
		b.mu.Unlock()
		return fmt.Errorf("could not attach the memory card buttons: %w", err)
	}
	return nil
}

// RepostPending cards every proposal still waiting on disk. The waiting map
// does not survive a restart; the proposals do, so this is how they become
// tappable again. Proposals from before entries existed cannot be applied and
// are shelved instead of carded. A proposal file that will not load is
// reported and left where it is; the rest are still carded.
func (b *Bot) RepostPending(ctx context.Context) error {
	ready, legacy, skipped, err := b.mem.Pending()
	if err != nil {
		return err
	}
	var first error
	for _, err := range skipped {
		fmt.Fprintf(os.Stderr, "approver bot: could not load a pending proposal: %v; it stays in .proposals\n", err)
		if first == nil {
			first = err
		}
	}
	for _, id := range legacy {
		if err := b.mem.MoveLegacy(id); err != nil {
			fmt.Fprintf(os.Stderr, "approver bot: could not shelve legacy proposal %s: %v\n", id, err)
			if first == nil {
				first = err
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "approver bot: shelved legacy proposal %s under .proposals/legacy\n", id)
	}
	for _, p := range ready {
		if err := b.ProposeMemory(ctx, p); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func memoryButtons(msgID int64) [][]Button {
	tok := callbackToken(msgID)
	return [][]Button{{
		{Text: "✅ approve", Data: "mem-approve:" + tok},
		{Text: "❌ reject", Data: "mem-reject:" + tok},
	}}
}

// memoryTap resolves a memory card. The id is claimed by removing it from
// the map before any work, so a second tap racing this one sees "already
// answered"; it is put back on any refusal or failure, so a retry — after
// /resume, say — still finds it.
func (b *Bot) memoryTap(ctx context.Context, c *Callback, approve bool) {
	msgID := c.Message.ID
	b.mu.Lock()
	carded, ok := b.memWaiting[msgID]
	if ok {
		delete(b.memWaiting, msgID)
	}
	b.mu.Unlock()
	if !ok {
		b.tg.AnswerCallback(ctx, c.ID, "already answered")
		return
	}
	id := carded.ID
	release := func() {
		b.mu.Lock()
		b.memWaiting[msgID] = carded
		b.mu.Unlock()
	}
	now := time.Now()

	// Fail closed: once an approver audit record has failed to land, the
	// ledger has a gap, and neither a write nor a reject's move is made
	// without a record. Both verbs are refused until a restart.
	if b.auditBroken.Load() {
		release()
		fmt.Fprintf(os.Stderr, "CRITICAL: approver refused a memory tap on proposal %s: the audit log is not writable\n", id)
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, "the approver cannot write its audit log; restart the gateway")
		return
	}

	if !approve {
		p, err := b.mem.Reject(id, now)
		if err != nil {
			release()
			// memory.ErrAlreadyWritten lands here too: its text is the card's
			// "already written; approve to finish filing it", and the buttons
			// stay so approve can do exactly that.
			b.memoryRecord(id, carded.Path, "deny", "memory.tap_rejected", "rejected by the approver",
				"denied_by_user", audit.Result{Status: "error", Error: err.Error()})
			b.tg.AnswerCallback(ctx, c.ID, "not rejected")
			b.memoryFailed(ctx, msgID, err.Error())
			return
		}
		b.memoryRecord(id, p.Path, "deny", "memory.tap_rejected", "rejected by the approver",
			"denied_by_user", audit.Result{Status: "refused"})
		b.tg.AnswerCallback(ctx, c.ID, "rejected")
		b.tg.EditMessage(ctx, b.chatID, msgID, fmt.Sprintf("❌ rejected: <code>%s</code>: %s",
			html.EscapeString(p.Path), html.EscapeString(p.Entry)), nil)
		return
	}

	// The tap is the human approval, so NeedApproval (taint) is satisfied by
	// it. Everything that denies outright — the kill switch, the rate limit —
	// still applies: a tap does not outrank /stop.
	d := policy.Decide(policy.Request{
		Tool: memoryTool, Known: true, Tier: policy.TierInternal, Kind: policy.Write,
		ArgsOK: true, ValidatorOK: true,
	}, b.state.Snapshot(now), b.cfg, now)
	if d.Outcome == policy.Deny {
		release()
		b.memoryRecord(id, carded.Path, "deny", d.RuleID, d.Reason,
			"approved", audit.Result{Status: "refused"})
		// The card keeps its buttons: once the reason is lifted, the same
		// card is tapped again.
		b.tg.AnswerCallback(ctx, c.ID, tapAnswer(d.Reason))
		return
	}

	// What the human approved is the card, not the file. A proposal changed
	// on disk since the card went out is refused rather than applied, so the
	// approval cannot be carried over to words nobody was shown.
	if cur, err := b.mem.Load(id); err != nil || cur.Path != carded.Path || cur.Entry != carded.Entry {
		reason := "the proposal changed after this card was shown"
		if err != nil {
			reason = err.Error()
		}
		release()
		b.memoryRecord(id, carded.Path, "deny", "memory.proposal_changed", reason,
			"approved", audit.Result{Status: "error", Error: reason})
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, reason)
		return
	}

	// Fail closed: a call the rate limit cannot count is not made.
	if err := b.state.RecordCall(memoryTool, now); err != nil {
		release()
		b.memoryRecord(id, carded.Path, "deny", "state.unwritable", "the gateway cannot record this call",
			"approved", audit.Result{Status: "error", Error: err.Error()})
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, "the gateway cannot record this call")
		return
	}

	// The ledger says a write is about to happen before it does. If that
	// record cannot land, nothing is written: a document must never hold a
	// line the ledger has no record of. A crash after this record and
	// before the result leaves an intent with no outcome, which reads as
	// "look at the document", not as silence.
	if err := b.memoryRecord(id, carded.Path, "allow", "memory.tap_approved", "approved by the approver",
		"approved", audit.Result{Status: "pending"}); err != nil {
		release()
		fmt.Fprintf(os.Stderr, "CRITICAL: approver did not write proposal %s: its intent record failed: %v\n", id, err)
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, "the approver cannot write its audit log; restart the gateway")
		return
	}

	p, err := b.mem.ApplyApproved(id, now)
	if err != nil {
		release()
		b.memoryRecord(id, carded.Path, "allow", "memory.tap_approved", "approved by the approver",
			"approved", audit.Result{Status: "error", Error: err.Error()})
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, err.Error())
		return
	}
	b.memoryRecord(id, p.Path, "allow", "memory.tap_approved", "approved by the approver",
		"approved", audit.Result{Status: "ok"})
	b.tg.AnswerCallback(ctx, c.ID, "written")
	b.tg.EditMessage(ctx, b.chatID, msgID, fmt.Sprintf("✅ written to <code>%s</code>: %s",
		html.EscapeString(p.Path), html.EscapeString(p.Entry)), nil)
}

// memoryFailed edits a card to say nothing was written. The reason can carry
// a path or file content, so it is truncated and escaped like any agent text.
// The buttons go back on with the same tokens: the proposal is still pending
// and the id is back in the map, so once the cause is fixed the same card is
// tapped again.
func (b *Bot) memoryFailed(ctx context.Context, msgID int64, reason string) {
	b.tg.EditMessage(ctx, b.chatID, msgID, "⚠️ not written: "+html.EscapeString(truncate(reason, 200)), memoryButtons(msgID))
}

// memoryRecord writes a tap's audit record. The args never carry the entry:
// the ledger says which proposal and which document, and the proposal file
// in applied/ or rejected/ holds the words.
func (b *Bot) memoryRecord(id, path, decision, rule, reason, answer string, res audit.Result) error {
	return b.appendRecord(audit.Record{
		Actor: "approver", Tool: memoryTool, Kind: policy.Write.String(), Tier: int(policy.TierInternal),
		Args:     audit.MarshalArgs(map[string]any{"proposal_id": id, "path": path}),
		Decision: decision, Rule: rule, Reason: reason,
		Approval: &audit.Approval{Answer: answer},
		Result:   res,
	})
}

// tapAnswer fits a reason into an answerCallbackQuery text. Telegram refuses
// one over 200 characters, and a refused answer leaves the tap spinning with
// no word at all. 199 bytes leaves room for the ellipsis truncate adds.
func tapAnswer(reason string) string {
	if utf8.RuneCountInString(reason) <= 200 {
		return reason
	}
	return truncate(reason, 199)
}
