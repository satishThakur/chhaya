package approval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	mu        sync.Mutex
	sent      []string
	updates   []Update
	sendErr   error
	getChatOK bool
	answered  []string
	// reply, if set, is the callback this fake produces the moment a card is sent.
	reply func(messageID int64) *Update
	out   chan Update
}

func newFakeTG() *fakeTG {
	return &fakeTG{getChatOK: true, out: make(chan Update, 16)}
}

func (f *fakeTG) GetMe(context.Context) (int64, string, error) { return 8940896193, "satish_chhaya_approver_bot", nil }

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

func TestApprovalTimeoutDenies(t *testing.T) {
	tg := newFakeTG() // reply is nil: nobody taps
	b, _, logPath := newTestBot(t, tg, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	approved, latency, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write", Reason: "tainted"})
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
