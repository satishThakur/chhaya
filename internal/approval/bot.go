package approval

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

// callbackSecret salts the token embedded in each button so that a
// callback_data value cannot be guessed or replayed across restarts.
var callbackSecret = func() []byte {
	b := make([]byte, 32)
	// A process that cannot obtain randomness must not serve approvals: a
	// weak or predictable secret here would let a forged callback_data value
	// pass the anti-replay check in tap(). Panicking at package init is
	// preferable to starting up with a silently weak secret on the kill
	// switch's approval path.
	if _, err := rand.Read(b); err != nil {
		panic("approval: could not seed callback secret: " + err.Error())
	}
	return b
}()

func callbackToken(messageID int64) string {
	m := hmac.New(sha256.New, callbackSecret)
	fmt.Fprintf(m, "%d", messageID)
	return hex.EncodeToString(m.Sum(nil))[:16]
}

type pending struct {
	ch    chan bool
	rule  string
	tool  string
	start time.Time
}

type Bot struct {
	tg      Telegram
	chatID  int64
	state   *state.Store
	audit   *audit.Writer
	timeout time.Duration

	mu      sync.Mutex
	waiting map[int64]*pending // keyed by the card's message id
}

// NewBot wires the approver bot. chatID is both the chat we post to and the
// only user whose taps count: in a private Telegram chat the chat id and the
// user id are the same number, and collapsing them means there is no way to
// configure a chat we listen to but do not trust.
func NewBot(tg Telegram, chatID int64, st *state.Store, w *audit.Writer, timeout time.Duration) *Bot {
	return &Bot{tg: tg, chatID: chatID, state: st, audit: w, timeout: timeout, waiting: map[int64]*pending{}}
}

// var _ mcpsrv.Approver forces a compile error the moment *Bot drifts from
// the interface mcpsrv.Gateway actually calls, rather than waiting for the
// daemon task that first wires the two together to find out.
var _ mcpsrv.Approver = (*Bot)(nil)

// Preflight is startup steps 3 and 4: the token must work, and the chat must
// be reachable. Either failing means the gateway does not start.
func (b *Bot) Preflight(ctx context.Context) error {
	if _, _, err := b.tg.GetMe(ctx); err != nil {
		return fmt.Errorf("approver bot token rejected: %w", err)
	}
	if err := b.tg.GetChat(ctx, b.chatID); err != nil {
		return fmt.Errorf("approver chat %d unreachable; press Start on the approver bot: %w", b.chatID, err)
	}
	return nil
}

// SendStartupCard posts only on first boot or when the policy changed. A card
// you see on every bounce is one you stop reading, and this bot's messages
// must stay worth reading.
func (b *Bot) SendStartupCard(ctx context.Context, policyHash string, firstBoot bool) error {
	why := "policy changed"
	if firstBoot {
		why = "first boot"
	}
	_, err := b.tg.SendMessage(ctx, b.chatID,
		fmt.Sprintf("<b>chhaya gateway up</b> (%s)\npolicy <code>%s</code>\n/status for caps and switches", why, truncate(policyHash, 12)), nil)
	return err
}

// Run long-polls for commands and button taps until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ups, err := b.tg.GetUpdates(ctx, offset, 25*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(2 * time.Second) // transient; keep polling
			continue
		}
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			b.handle(ctx, u)
		}
	}
}

func (b *Bot) handle(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		if u.Message.From.ID != b.chatID {
			b.auditRejectedSender(u.Message.From.ID, "message", u.Message.Text)
			return
		}
		b.command(ctx, strings.TrimSpace(u.Message.Text))
	case u.Callback != nil:
		if u.Callback.From.ID != b.chatID {
			b.auditRejectedSender(u.Callback.From.ID, "callback", u.Callback.Data)
			return
		}
		b.tap(ctx, u.Callback)
	}
}

func (b *Bot) command(ctx context.Context, text string) {
	now := time.Now()
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return
	}
	// A prefix match on the raw text would fire the kill switch on "/stopped"
	// as readily as on "/stop", which is a bad place for imprecise parsing
	// even though only the trusted approver can reach this path. Parse the
	// first whitespace-separated field and switch on it exactly. Telegram
	// also sends "/stop@botname" in group chats; strip the "@suffix" so that
	// form still matches, even though group join is disabled today.
	cmd, _, _ := strings.Cut(fields[0], "@")
	switch cmd {
	case "/stop":
		if err := b.state.SetKillSwitch(true, now); err != nil {
			b.tg.SendMessage(ctx, b.chatID, "could not persist the kill switch: "+err.Error(), nil)
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>stopped.</b> Writes are refused. Reads still work so it can tell you why. /resume to lift.", nil)

	case "/resume":
		if err := b.state.SetKillSwitch(false, now); err != nil {
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>resumed.</b> Writes are allowed again.", nil)

	case "/clear":
		if err := b.state.SetTaint(false, "", now); err != nil {
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>taint cleared.</b> Writes no longer need approval on that account.", nil)

	case "/status":
		st := b.state.Snapshot(now)
		var sb strings.Builder
		fmt.Fprintf(&sb, "kill switch: <b>%s</b>\n", onOff(st.KillSwitch))
		if st.Tainted {
			fmt.Fprintf(&sb, "tainted: <b>yes</b>, from %s at %s\n", st.TaintSource, st.TaintedAt.UTC().Format(time.RFC3339))
		} else {
			sb.WriteString("tainted: no\n")
		}
		for k, v := range st.Spent {
			fmt.Fprintf(&sb, "spent on %s today: ₹%d\n", k, v/100)
		}
		b.tg.SendMessage(ctx, b.chatID, sb.String(), nil)
	}
}

func (b *Bot) tap(ctx context.Context, c *Callback) {
	parts := strings.SplitN(c.Data, ":", 2)
	if len(parts) != 2 || c.Message == nil {
		return
	}
	if !hmac.Equal([]byte(parts[1]), []byte(callbackToken(c.Message.ID))) {
		b.tg.AnswerCallback(ctx, c.ID, "that card has expired")
		return
	}

	b.mu.Lock()
	p, ok := b.waiting[c.Message.ID]
	if ok {
		delete(b.waiting, c.Message.ID)
	}
	b.mu.Unlock()
	if !ok {
		b.tg.AnswerCallback(ctx, c.ID, "already answered")
		return
	}

	approved := parts[0] == "approve"
	b.tg.AnswerCallback(ctx, c.ID, map[bool]string{true: "approved", false: "denied"}[approved])
	p.ch <- approved
}

// Request implements mcpsrv.Approver.
func (b *Bot) Request(ctx context.Context, call mcpsrv.ToolCall, d policy.Decision) (bool, time.Duration, error) {
	start := time.Now()

	// ONE message carrying both the card and its keyboard. Two messages would
	// put the buttons on a different message id than the token was minted
	// from, so every tap would read as expired — and two notifications per
	// approval is noise on a bot whose messages must stay worth reading.
	// The token cannot be minted before the id exists, so it is sent empty
	// and the keyboard is attached by editing the message we just posted.
	text := b.card(call, d)
	msgID, err := b.tg.SendMessage(ctx, b.chatID, text, nil)
	if err != nil {
		// Do not guess. Surfacing the error lets Invoke write the CRITICAL
		// record and tell Hermes the channel is down.
		return false, time.Since(start), fmt.Errorf("could not send the approval card: %w", err)
	}
	// The pending entry goes into the map before the keyboard is attached,
	// not after. Attaching the keyboard is what makes a tap possible — on a
	// real tap that is the earliest moment one could arrive, and in a test
	// double that fabricates the tap synchronously from inside
	// AttachKeyboard, it is the exact moment one does. Registering the
	// waiter first means there is no window where a tap can arrive before
	// this function is ready to receive it.
	p := &pending{ch: make(chan bool, 1), rule: d.RuleID, tool: call.Name, start: start}
	b.mu.Lock()
	b.waiting[msgID] = p
	b.mu.Unlock()

	tok := callbackToken(msgID)
	if err := b.tg.AttachKeyboard(ctx, b.chatID, msgID, [][]Button{{
		{Text: "✅ approve", Data: "approve:" + tok},
		{Text: "❌ deny", Data: "deny:" + tok},
	}}); err != nil {
		b.mu.Lock()
		delete(b.waiting, msgID)
		b.mu.Unlock()
		return false, time.Since(start), fmt.Errorf("could not attach the approval buttons: %w", err)
	}

	select {
	case ok := <-p.ch:
		return ok, time.Since(start), nil

	case <-time.After(b.timeout):
		b.mu.Lock()
		delete(b.waiting, msgID)
		b.mu.Unlock()
		// A timeout needs a trace as loud as a tap, and it is this package's
		// event, not Invoke's: Invoke only sees "not approved".
		b.appendRecord(audit.Record{
			Actor: "approver", Tool: call.Name, Kind: "write", Tier: int(d.Tier),
			Args: audit.MarshalArgs(map[string]any{}), Decision: "deny", Rule: d.RuleID,
			Reason:   "no answer within " + b.timeout.String(),
			Approval: &audit.Approval{Answer: "timeout", LatencyMS: time.Since(start).Milliseconds()},
			Result:   audit.Result{Status: "refused"},
		})
		b.tg.SendMessage(ctx, b.chatID, "⏱ no answer in time — <b>denied</b>: "+call.Name, nil)
		return false, time.Since(start), nil

	case <-ctx.Done():
		// The map entry must not outlive the request: without this delete, a
		// cancelled Request (the gateway's own backstop timeout, or the
		// caller giving up) leaves a pending approval in the map that
		// nothing will ever remove — a late tap would still find it and
		// write to the buffered channel, but no one is left reading it, and
		// the map slot itself leaks for the life of the process.
		b.mu.Lock()
		delete(b.waiting, msgID)
		b.mu.Unlock()
		return false, time.Since(start), ctx.Err()
	}
}

func (b *Bot) card(call mcpsrv.ToolCall, d policy.Decision) string {
	var sb strings.Builder
	sb.WriteString("<b>approval needed</b>\n")
	fmt.Fprintf(&sb, "tool: <code>%s</code>\n", call.Name)
	fmt.Fprintf(&sb, "tier: %d\n", d.Tier)
	fmt.Fprintf(&sb, "rule: <code>%s</code>\n", d.RuleID)
	if d.Reason != "" {
		fmt.Fprintf(&sb, "why: %s\n", d.Reason)
	}
	if len(call.Args) > 0 {
		shown, _ := json.Marshal(audit.Redact(call.Args, d.Redact))
		fmt.Fprintf(&sb, "args: <code>%s</code>", truncate(string(shown), 500))
	}
	return sb.String()
}

func (b *Bot) auditRejectedSender(senderID int64, kind, payload string) {
	args := audit.MarshalArgs(map[string]any{"sender_id": senderID, "update_kind": kind, "payload": truncate(payload, 120)})
	b.appendRecord(audit.Record{
		Actor: "unknown", Tool: "approver." + kind, Kind: "write", Args: args,
		Decision: "deny", Rule: "approver.unauthorized_sender",
		Reason:   fmt.Sprintf("telegram user %d is not the configured approver", senderID),
		Result:   audit.Result{Status: "refused"},
		Severity: "critical",
	})
}

func (b *Bot) appendRecord(r audit.Record) {
	if _, err := b.audit.Append(r); err != nil {
		fmt.Printf("CRITICAL: approver could not write an audit record: %v\n", err)
	}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "off"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
