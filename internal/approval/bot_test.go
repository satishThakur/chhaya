package approval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

const allowedUser = int64(8726466304)

type fakeTG struct {
	mu            sync.Mutex
	sent          []string
	updates       []Update
	sendErr       error
	getChatOK     bool
	answered      []string
	getUpdatesErr error // non-nil: GetUpdates always fails, for the channel-unreachable test
	// reply, if set, is the callback this fake produces the moment a card is sent.
	reply func(messageID int64) *Update
	out   chan Update
}

func newFakeTG() *fakeTG {
	return &fakeTG{getChatOK: true, out: make(chan Update, 16)}
}

func (f *fakeTG) GetMe(context.Context) (int64, string, error) {
	return 8940896193, "satish_chhaya_approver_bot", nil
}

func (f *fakeTG) GetChat(_ context.Context, _ int64) error {
	if !f.getChatOK {
		return errors.New("chat not found")
	}
	return nil
}

func (f *fakeTG) SendMessage(_ context.Context, _ int64, text string, _ [][]Button) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.mu.Lock()
	f.sent = append(f.sent, text)
	id := int64(len(f.sent))
	f.mu.Unlock()
	return id, nil
}

// AttachKeyboard is where the fake produces its tap, because that is the
// point at which the real bot has a message id and a matching token.
func (f *fakeTG) AttachKeyboard(_ context.Context, _ int64, messageID int64, _ [][]Button) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if f.reply != nil {
		if u := f.reply(messageID); u != nil {
			f.out <- *u
		}
	}
	return nil
}

func (f *fakeTG) AnswerCallback(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, id)
	return nil
}

func (f *fakeTG) GetUpdates(ctx context.Context, _ int64, timeout time.Duration) ([]Update, error) {
	f.mu.Lock()
	err := f.getUpdatesErr
	f.mu.Unlock()
	if err != nil {
		// Fails immediately rather than waiting out timeout, so a test can
		// drive many failures quickly instead of waiting a real long poll.
		return nil, err
	}
	select {
	case u := <-f.out:
		return []Update{u}, nil
	case <-time.After(timeout):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTG) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

func newTestBot(t *testing.T, tg Telegram, timeout time.Duration) (*Bot, *state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "audit.jsonl")
	w, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	b := NewBot(tg, allowedUser, st, w, timeout)
	return b, st, logPath
}

func cmd(text string) Update {
	return Update{ID: time.Now().UnixNano(), Message: &Message{From: User{ID: allowedUser}, Chat: Chat{ID: allowedUser}, Text: text}}
}

func TestStopAndResumeDriveTheKillSwitch(t *testing.T) {
	tg := newFakeTG()
	b, st, _ := newTestBot(t, tg, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	tg.out <- cmd("/stop")
	waitFor(t, func() bool { return st.Snapshot(time.Now()).KillSwitch })

	tg.out <- cmd("/resume")
	waitFor(t, func() bool { return !st.Snapshot(time.Now()).KillSwitch })
}

func TestClearClearsTaint(t *testing.T) {
	tg := newFakeTG()
	b, st, _ := newTestBot(t, tg, time.Second)
	st.SetTaint(true, "web.fetch", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	tg.out <- cmd("/clear")
	waitFor(t, func() bool { return !st.Snapshot(time.Now()).Tainted })
}

func TestAnUnauthorisedSenderIsIgnoredAndAudited(t *testing.T) {
	tg := newFakeTG()
	b, st, logPath := newTestBot(t, tg, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	u := cmd("/stop")
	u.Message.From.ID = 999 // not the configured user
	tg.out <- u

	waitFor(t, func() bool { return len(records(t, logPath)) > 0 })
	if st.Snapshot(time.Now()).KillSwitch {
		t.Fatal("a stranger pressed the kill switch")
	}
	last := records(t, logPath)[0]
	if last.Rule != "approver.unauthorized_sender" {
		t.Errorf("rule = %q", last.Rule)
	}
	if !strings.Contains(string(last.Args), "999") {
		t.Error("the record should say who it was, so we can see it happening")
	}
}

// TestAnUnauthorisedCallbackIsIgnoredAndAudited is the callback-side twin of
// TestAnUnauthorisedSenderIsIgnoredAndAudited. The two look symmetric by
// inspection, but an authorisation check nothing exercises is one that can
// silently stop working, and tap() is where a forged approval would land.
func TestAnUnauthorisedCallbackIsIgnoredAndAudited(t *testing.T) {
	tg := newFakeTG()
	b, _, logPath := newTestBot(t, tg, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	tg.out <- Update{ID: time.Now().UnixNano(), Callback: &Callback{
		ID: "cb-stranger", From: User{ID: 999}, Data: "approve:forged",
		Message: &Message{ID: 1, Chat: Chat{ID: allowedUser}},
	}}

	waitFor(t, func() bool { return len(records(t, logPath)) > 0 })
	if len(tg.answered) != 0 {
		t.Fatal("an unauthorised tap must never reach tap(); answering it would close a loop that was never opened")
	}
	last := records(t, logPath)[0]
	if last.Rule != "approver.unauthorized_sender" {
		t.Errorf("rule = %q", last.Rule)
	}
	if !strings.Contains(string(last.Args), "999") {
		t.Error("the record should say who it was, so we can see it happening")
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	tg := newFakeTG() // reply is nil: nobody taps
	b, _, logPath := newTestBot(t, tg, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	approved, latency, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, Tier: policy.TierMoney, RuleID: "taint.write", Reason: "tainted"})
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		t.Fatal("a timeout must deny")
	}
	if latency < 150*time.Millisecond {
		t.Errorf("latency = %v, should cover the wait", latency)
	}

	recs := records(t, logPath)
	if len(recs) == 0 {
		t.Fatal("a timeout needs a trace as loud as a tap")
	}
	last := recs[len(recs)-1]
	if last.Approval == nil || last.Approval.Answer != "timeout" {
		t.Errorf("record = %+v", last)
	}
	// A timed-out tier-2 approval must not read as a tier-0 internal action
	// in a permanent ledger.
	if last.Tier != int(policy.TierMoney) {
		t.Errorf("tier = %d, want %d: the decision that timed out was tier-2", last.Tier, int(policy.TierMoney))
	}
}

func TestApprovalTapIsHonoured(t *testing.T) {
	tg := newFakeTG()
	tg.reply = func(messageID int64) *Update {
		// Message must be populated: tap() keys the pending approval off
		// c.Message.ID, and a Callback without it is silently dropped.
		return &Update{ID: messageID, Callback: &Callback{
			ID: "cb1", From: User{ID: allowedUser}, Data: "approve:" + callbackToken(messageID),
			Message: &Message{ID: messageID, Chat: Chat{ID: allowedUser}},
		}}
	}
	b, _, _ := newTestBot(t, tg, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	approved, _, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write", Reason: "tainted"})
	if err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Fatal("the tap should have approved it")
	}
	if len(tg.answered) == 0 {
		t.Error("answerCallbackQuery closes the loop; without it the button spins forever")
	}
}

func TestACardStatesTheRuleThatFired(t *testing.T) {
	tg := newFakeTG()
	b, _, _ := newTestBot(t, tg, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.Request(ctx, mcpsrv.ToolCall{Name: "food.place_order", Args: map[string]any{"total_inr": 420}},
		policy.Decision{Outcome: policy.NeedApproval, Tier: policy.TierMoney, RuleID: "tier2.approval",
			Reason: "approval required: this spends money"})

	texts := tg.texts()
	if len(texts) == 0 {
		t.Fatal("no card was sent")
	}
	card := texts[0]
	for _, want := range []string{"food.place_order", "tier2.approval"} {
		if !strings.Contains(card, want) {
			t.Errorf("card does not mention %q:\n%s", want, card)
		}
	}
}

func TestAFailedSendIsReportedAsAnError(t *testing.T) {
	tg := newFakeTG()
	tg.sendErr = errors.New("forbidden: bot was blocked by the user")
	b, _, _ := newTestBot(t, tg, time.Second)

	_, _, err := b.Request(context.Background(), mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write"})
	if err == nil {
		t.Fatal("a card that could not be sent must surface as an error, not a silent deny")
	}
}

func TestPreflightRefusesAnUnreachableChat(t *testing.T) {
	tg := newFakeTG()
	tg.getChatOK = false
	b, _, _ := newTestBot(t, tg, time.Second)

	if err := b.Preflight(context.Background()); err == nil {
		t.Fatal("an unreachable approver chat must stop the gateway starting")
	}
}

// TestSendStartupCardDoesNotPanicOnAShortHash guards against a hash shorter
// than the 12-character prefix the startup card shows. A naive policyHash[:12]
// slice would panic on anything shorter, crashing the gateway on startup
// instead of just showing the whole (short) hash.
func TestSendStartupCardDoesNotPanicOnAShortHash(t *testing.T) {
	tg := newFakeTG()
	b, _, _ := newTestBot(t, tg, time.Second)

	if err := b.SendStartupCard(context.Background(), "abcd", true); err != nil {
		t.Fatal(err)
	}
	texts := tg.texts()
	if len(texts) == 0 || !strings.Contains(texts[0], "abcd") {
		t.Errorf("startup card does not show the short hash: %v", texts)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// TestChannelUnreachableRaisesACriticalRecord proves the fix for the dead
// error branch in Run: previously a continuously failing GetUpdates was
// retried forever with nothing logged and no record of it anywhere, which
// matters because /stop is delivered only through this loop. The threshold is
// injected on this *Bot (b.unreachableAfter), not shortened on the package
// default, so production's defaultUnreachableAfter is untouched by this test.
func TestChannelUnreachableRaisesACriticalRecord(t *testing.T) {
	tg := newFakeTG()
	tg.getUpdatesErr = errors.New("connection refused")
	b, _, logPath := newTestBot(t, tg, time.Second)
	b.unreachableAfter = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	// Run's retry sleep between failed polls is a fixed 2 seconds (unaffected
	// by unreachableAfter), so this needs more headroom than waitFor's 2
	// second deadline gives: the first failure cannot raise the alarm (zero
	// elapsed time cannot exceed any positive threshold), so the earliest
	// possible alarm is on the second failed attempt, after one full retry
	// sleep has elapsed.
	deadline := time.Now().Add(5 * time.Second)
	var recs []audit.Record
	for time.Now().Before(deadline) {
		recs = records(t, logPath)
		if len(recs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(recs) == 0 {
		t.Fatal("no audit record was written for a continuously unreachable approval channel")
	}
	last := recs[len(recs)-1]
	if last.Rule != "approver.channel_unreachable" {
		t.Errorf("rule = %q, want approver.channel_unreachable", last.Rule)
	}
	if last.Severity != "critical" {
		t.Errorf("severity = %q, want critical: a dead kill switch is a CRITICAL event", last.Severity)
	}
}

func records(t *testing.T, path string) []audit.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []audit.Record
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// tagSequence returns the HTML tags in s, in order. The card is sent with
// parse_mode: HTML, so this is the structure a human actually reads — and
// the thing no agent-controlled value may add to.
func tagSequence(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "<")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], ">")
		if j < 0 {
			return append(out, s[i:])
		}
		out = append(out, s[i:i+j+1])
		s = s[i+j+1:]
	}
}

// TestTheCardCannotBeForgedByAgentControlledText: the card is HTML, and a
// human approves on the strength of what it says. If a value the agent
// controls can close a tag and open its own, it can show the approver
// "tool: memory.get" over a call that is something else entirely.
//
// Only the reason and rule-id cases flip against the pre-fix code; the
// argument case passed already, but only because card() happened to use
// json.Marshal, which HTML-escapes by default. audit.MarshalArgs deliberately
// does not, so the "use one marshaller for both" cleanup a reviewer would ask
// for is exactly what this case is here to catch.
func TestTheCardCannotBeForgedByAgentControlledText(t *testing.T) {
	b, _, _ := newTestBot(t, newFakeTG(), time.Second)
	const forge = "</code><b>tool: memory.get</b><code>"

	benign := b.card(
		mcpsrv.ToolCall{Name: "memory.propose_update", Args: map[string]any{"diff": "+ a line"}},
		policy.Decision{Outcome: policy.NeedApproval, Tier: policy.TierReversible, RuleID: "taint.write", Reason: "untrusted content entered the session"})
	want := tagSequence(benign)
	if len(want) == 0 {
		t.Fatal("precondition: the card should contain HTML tags at all")
	}

	cases := map[string]struct {
		call mcpsrv.ToolCall
		d    policy.Decision
	}{
		"an argument": {
			mcpsrv.ToolCall{Name: "memory.propose_update", Args: map[string]any{"diff": forge}},
			policy.Decision{RuleID: "taint.write", Reason: "untrusted content entered the session"},
		},
		"the reason": {
			mcpsrv.ToolCall{Name: "memory.propose_update", Args: map[string]any{"diff": "+ a line"}},
			policy.Decision{RuleID: "taint.write", Reason: forge},
		},
		"the rule id": {
			mcpsrv.ToolCall{Name: "memory.propose_update", Args: map[string]any{"diff": "+ a line"}},
			policy.Decision{RuleID: forge, Reason: "untrusted content entered the session"},
		},
		"the tool name": {
			mcpsrv.ToolCall{Name: forge, Args: map[string]any{"diff": "+ a line"}},
			policy.Decision{RuleID: "taint.write", Reason: "untrusted content entered the session"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := b.card(tc.call, tc.d)
			if strings.Contains(got, "<b>tool:") {
				t.Errorf("%s forged a line the approver reads as the gateway's own:\n%s", name, got)
			}
			if gotTags := tagSequence(got); !slices.Equal(gotTags, want) {
				t.Errorf("%s changed the card's markup.\n got: %v\nwant: %v\ncard:\n%s", name, gotTags, want, got)
			}
			if strings.Contains(got, forge) {
				t.Errorf("%s reached the card as live markup:\n%s", name, got)
			}
			// Escaped, not dropped: the approver still gets to see what the
			// agent actually sent. The two escapes differ — html.EscapeString
			// writes &lt;, json.Marshal writes \u003c — so assert on the part
			// neither of them touches.
			if !strings.Contains(got, "memory.get") {
				t.Errorf("%s: the hostile text should be escaped, not hidden:\n%s", name, got)
			}
		})
	}
}

// TestTheTimeoutRecordStatesTheCallsRealKind: the timeout record used to
// hard-code Kind: "write". Everything else in a record comes from the
// decision or the registration, and a permanent ledger that calls a read a
// write is wrong in the same way a wrong tier is.
func TestTheTimeoutRecordStatesTheCallsRealKind(t *testing.T) {
	for _, kind := range []policy.Kind{policy.Read, policy.Write} {
		t.Run(kind.String(), func(t *testing.T) {
			tg := newFakeTG() // reply is nil: nobody taps
			b, _, logPath := newTestBot(t, tg, 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go b.Run(ctx)

			if _, _, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.call"},
				policy.Decision{Outcome: policy.NeedApproval, Tier: policy.TierMoney, Kind: kind,
					RuleID: "tier2.approval", Reason: "approval required"}); err != nil {
				t.Fatal(err)
			}

			recs := records(t, logPath)
			if len(recs) == 0 {
				t.Fatal("a timeout needs a trace as loud as a tap")
			}
			last := recs[len(recs)-1]
			if last.Approval == nil || last.Approval.Answer != "timeout" {
				t.Fatalf("record = %+v, want the timeout record", last)
			}
			if last.Kind != kind.String() {
				t.Errorf("kind = %q, want %q: it must come from the decision, not a literal", last.Kind, kind.String())
			}
		})
	}
}
