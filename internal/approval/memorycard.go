package approval

import (
	"context"
	"fmt"
	"html"
	"os"
	"time"

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
	// Request: attaching it is the earliest moment a tap can arrive.
	b.mu.Lock()
	b.memWaiting[msgID] = p.ID
	b.mu.Unlock()

	tok := callbackToken(msgID)
	if err := b.tg.AttachKeyboard(ctx, b.chatID, msgID, [][]Button{{
		{Text: "✅ approve", Data: "mem-approve:" + tok},
		{Text: "❌ reject", Data: "mem-reject:" + tok},
	}}); err != nil {
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
// are shelved instead of carded.
func (b *Bot) RepostPending(ctx context.Context) error {
	ready, legacy, err := b.mem.Pending()
	if err != nil {
		return err
	}
	var first error
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

// memoryTap resolves a memory card. The id is claimed by removing it from
// the map before any work, so a second tap racing this one sees "already
// answered"; it is put back on any refusal or failure, so a retry — after
// /resume, say — still finds it.
func (b *Bot) memoryTap(ctx context.Context, c *Callback, approve bool) {
	msgID := c.Message.ID
	b.mu.Lock()
	id, ok := b.memWaiting[msgID]
	if ok {
		delete(b.memWaiting, msgID)
	}
	b.mu.Unlock()
	if !ok {
		b.tg.AnswerCallback(ctx, c.ID, "already answered")
		return
	}
	release := func() {
		b.mu.Lock()
		b.memWaiting[msgID] = id
		b.mu.Unlock()
	}
	now := time.Now()

	if !approve {
		p, err := b.mem.Reject(id, now)
		if err != nil {
			release()
			b.memoryRecord(id, b.proposalPath(id), "deny", "memory.tap_rejected", "rejected by the approver",
				"denied_by_user", audit.Result{Status: "error", Error: err.Error()})
			b.tg.AnswerCallback(ctx, c.ID, "not rejected")
			b.memoryFailed(ctx, msgID, err.Error())
			return
		}
		b.memoryRecord(id, p.Path, "deny", "memory.tap_rejected", "rejected by the approver",
			"denied_by_user", audit.Result{Status: "refused"})
		b.tg.AnswerCallback(ctx, c.ID, "rejected")
		b.tg.EditMessage(ctx, b.chatID, msgID, fmt.Sprintf("❌ rejected: <code>%s</code>: %s",
			html.EscapeString(p.Path), html.EscapeString(p.Entry)))
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
		b.memoryRecord(id, b.proposalPath(id), "deny", d.RuleID, d.Reason,
			"approved", audit.Result{Status: "refused"})
		// The card keeps its buttons: once the reason is lifted, the same
		// card is tapped again.
		b.tg.AnswerCallback(ctx, c.ID, d.Reason)
		return
	}

	// Fail closed: a call the rate limit cannot count is not made.
	if err := b.state.RecordCall(memoryTool, now); err != nil {
		release()
		b.memoryRecord(id, b.proposalPath(id), "deny", "state.unwritable", "the gateway cannot record this call",
			"approved", audit.Result{Status: "error", Error: err.Error()})
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, "the gateway cannot record this call")
		return
	}

	p, err := b.mem.ApplyApproved(id, now)
	if err != nil {
		release()
		b.memoryRecord(id, b.proposalPath(id), "allow", "memory.tap_approved", "approved by the approver",
			"approved", audit.Result{Status: "error", Error: err.Error()})
		b.tg.AnswerCallback(ctx, c.ID, "not written")
		b.memoryFailed(ctx, msgID, err.Error())
		return
	}
	b.memoryRecord(id, p.Path, "allow", "memory.tap_approved", "approved by the approver",
		"approved", audit.Result{Status: "ok"})
	b.tg.AnswerCallback(ctx, c.ID, "written")
	b.tg.EditMessage(ctx, b.chatID, msgID, fmt.Sprintf("✅ written to <code>%s</code>: %s",
		html.EscapeString(p.Path), html.EscapeString(p.Entry)))
}

// memoryFailed edits a card to say nothing was written. The reason can carry
// a path or file content, so it is truncated and escaped like any agent text.
func (b *Bot) memoryFailed(ctx context.Context, msgID int64, reason string) {
	b.tg.EditMessage(ctx, b.chatID, msgID, "⚠️ not written: "+html.EscapeString(truncate(reason, 200)))
}

// memoryRecord writes a tap's audit record. The args never carry the entry:
// the ledger says which proposal and which document, and the proposal file
// in applied/ or rejected/ holds the words.
func (b *Bot) memoryRecord(id, path, decision, rule, reason, answer string, res audit.Result) {
	b.appendRecord(audit.Record{
		Actor: "approver", Tool: memoryTool, Kind: policy.Write.String(), Tier: int(policy.TierInternal),
		Args:     audit.MarshalArgs(map[string]any{"proposal_id": id, "path": path}),
		Decision: decision, Rule: rule, Reason: reason,
		Approval: &audit.Approval{Answer: answer},
		Result:   res,
	})
}

// proposalPath is the document a still-pending proposal names, for the audit
// record of a tap that did not resolve it. Empty when the file is unreadable,
// which is itself the usual reason the tap failed.
func (b *Bot) proposalPath(id string) string {
	p, err := b.mem.Load(id)
	if err != nil {
		return ""
	}
	return p.Path
}
