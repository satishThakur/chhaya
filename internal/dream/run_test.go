package dream

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/trace"
)

func mkTrace(id, source, last string, msgs ...trace.Message) *trace.Trace {
	t := &trace.Trace{ID: id, Source: source, Day: "2026-11-16", Messages: msgs}
	t.LastMessage = ts(last)
	return t
}

func ownerSays(id int64, text string) trace.Message {
	return trace.Message{ID: id, Role: "user", Content: text}
}

func TestRunSkipsWhatIsNotAnOwnerConversationOrAlreadyDone(t *testing.T) {
	led := &Ledger{path: filepath.Join(t.TempDir(), "l.json"), Done: map[string]string{}}
	cron := mkTrace("c1", "cron", "2026-11-16T01:00:00Z", ownerSays(1, "digest prompt"))
	botOnly := mkTrace("b1", "telegram", "2026-11-16T01:00:00Z", trace.Message{ID: 1, Role: "user", Content: "[System note: x]"})
	real := mkTrace("t1", "telegram", "2026-11-16T02:00:00Z", ownerSays(1, "I already heard Foo"))
	g := gw()
	m := &script{text: "nothing to save"}
	var out bytes.Buffer
	sum, err := RunTraces(context.Background(), m, g, []*trace.Trace{cron, botOnly, real}, RunOptions{Budget: 5, Propose: true, Ledger: led}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Read != 1 || sum.Skipped != 2 {
		t.Fatalf("%+v\n%s", sum, out.String())
	}
	// a second run over the same, unchanged conversation reads nothing
	m2 := &script{text: "x"}
	sum, _ = RunTraces(context.Background(), m2, g, []*trace.Trace{real}, RunOptions{Budget: 5, Propose: true, Ledger: led}, &out)
	if sum.Read != 0 || len(m2.seen) != 0 {
		t.Fatalf("an unchanged conversation must not be read twice: %+v", sum)
	}
	// a resumed conversation (a later last message) is read again
	real2 := mkTrace("t1", "telegram", "2026-11-16T03:00:00Z", ownerSays(1, "I already heard Foo"))
	if sum, _ = RunTraces(context.Background(), &script{text: "x"}, g, []*trace.Trace{real2}, RunOptions{Budget: 5, Propose: true, Ledger: led}, &out); sum.Read != 1 {
		t.Fatalf("%+v", sum)
	}
}

func TestDryRunProposesNothingAndLedgersNothing(t *testing.T) {
	led := &Ledger{path: filepath.Join(t.TempDir(), "l.json"), Done: map[string]string{}}
	tr := mkTrace("t1", "telegram", "2026-11-16T02:00:00Z", ownerSays(1, "I already heard Foo"))
	g := gw()
	m := play(prop("1", "interests", "Has already heard Foo.", 1))
	var out bytes.Buffer
	sum, err := RunTraces(context.Background(), m, g, []*trace.Trace{tr}, RunOptions{Budget: 5, Propose: false, Ledger: led}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 0 || len(led.Done) != 0 {
		t.Fatalf("dry run touched something: proposed %v ledger %v", g.proposed, led.Done)
	}
	if sum.Proposed != 1 || !strings.Contains(out.String(), "Has already heard Foo.") || !strings.Contains(out.String(), "dry run") {
		t.Fatalf("a dry run must show what it would send:\n%s", out.String())
	}
}

func TestBudgetIsSharedAcrossConversationsAndTheRestWaitForTomorrow(t *testing.T) {
	led := &Ledger{path: filepath.Join(t.TempDir(), "l.json"), Done: map[string]string{}}
	a := mkTrace("a", "telegram", "2026-11-16T01:00:00Z", ownerSays(1, "x"))
	b := mkTrace("b", "telegram", "2026-11-16T02:00:00Z", ownerSays(1, "y"))
	g := gw()
	m := &script{turns: [][]memrun.ToolCall{
		{prop("1", "interests", "Fact one.", 1), prop("2", "interests", "Fact two.", 1)},
	}, text: "done"}
	var out bytes.Buffer
	sum, err := RunTraces(context.Background(), m, g, []*trace.Trace{a, b}, RunOptions{Budget: 2, Propose: true, Ledger: led}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 2 || sum.Read != 1 || sum.Deferred != 1 {
		t.Fatalf("%+v proposed %d\n%s", sum, len(g.proposed), out.String())
	}
	if _, done := led.Done["b"]; done {
		t.Fatal("a conversation that was not read must not be marked done")
	}
}

func TestAnAgentErrorLeavesTheConversationForTheNextRun(t *testing.T) {
	led := &Ledger{path: filepath.Join(t.TempDir(), "l.json"), Done: map[string]string{}}
	tr := mkTrace("t1", "telegram", "2026-11-16T02:00:00Z", ownerSays(1, "x"))
	loop := &script{text: "x"}
	for i := 0; i < 30; i++ {
		loop.turns = append(loop.turns, []memrun.ToolCall{tc("l", "memory_list", map[string]any{})})
	}
	var out bytes.Buffer
	sum, err := RunTraces(context.Background(), loop, gw(), []*trace.Trace{tr}, RunOptions{Budget: 5, Propose: true, Ledger: led}, &out)
	if err == nil || sum.Errors != 1 || len(led.Done) != 0 {
		t.Fatalf("%+v %v %v", sum, err, led.Done)
	}
}

func TestLedgerSurvivesARestart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l.json")
	l, err := OpenLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Mark("t1", "2026-11-16T02:00:00Z"); err != nil {
		t.Fatal(err)
	}
	l2, err := OpenLedger(p)
	if err != nil || !l2.Seen("t1", "2026-11-16T02:00:00Z") || l2.Seen("t1", "2026-11-16T03:00:00Z") {
		t.Fatalf("%v %+v", err, l2)
	}
}

func TestWhatOneConversationProposedIsToldToTheNextAgentInTheSameRun(t *testing.T) {
	g := &fakeGW{docs: map[string]string{"interests": "# Interests\n"}}
	// One scripted model serves both conversations: its first turn proposes, every later turn ends.
	m := &script{turns: [][]memrun.ToolCall{{tc("1", "propose_fact", map[string]any{
		"path": "interests", "entry": "Is attending a Lumen Drift concert in Pune.", "rationale": "said so", "source": []int64{1}})}}, text: "done"}
	t1 := mkTrace("a", "telegram", "2026-11-20T08:00:00Z", ownerSays(1, "I am going to the Lumen Drift concert in Pune"))
	t2 := mkTrace("b", "telegram", "2026-11-20T09:00:00Z", ownerSays(1, "Off to see Lumen Drift live tonight"))
	var out bytes.Buffer
	if _, err := RunTraces(context.Background(), m, g, []*trace.Trace{t1, t2}, RunOptions{Budget: 5, Propose: true}, &out); err != nil {
		t.Fatal(err)
	}
	// calls: 0 = first conversation proposes, 1 = it ends, 2 = the second conversation starts
	if len(m.seen) < 3 {
		t.Fatalf("both conversations must be read; the model was called %d time(s)", len(m.seen))
	}
	if strings.Contains(m.seen[0][0].Content, "Already proposed earlier") {
		t.Fatal("the first agent has nothing prior and must not be told so")
	}
	sys := m.seen[2][0].Content
	if !strings.Contains(sys, "Already proposed earlier in this same run") || !strings.Contains(sys, "[interests] Is attending a Lumen Drift concert in Pune.") {
		t.Fatalf("the second agent must be told what is already waiting for a tap:\n%s", sys)
	}
}

func TestADryRunTellsLaterAgentsToo(t *testing.T) {
	g := &fakeGW{docs: map[string]string{"interests": "# Interests\n"}}
	m := &script{turns: [][]memrun.ToolCall{{tc("1", "propose_fact", map[string]any{
		"path": "interests", "entry": "Owns a Lumen Drift vinyl.", "rationale": "said so", "source": []int64{1}})}}, text: "done"}
	t1 := mkTrace("a", "telegram", "2026-11-20T08:00:00Z", ownerSays(1, "I own a Lumen Drift vinyl"))
	t2 := mkTrace("b", "telegram", "2026-11-20T09:00:00Z", ownerSays(1, "my Lumen Drift record is lovely"))
	var out bytes.Buffer
	if _, err := RunTraces(context.Background(), m, g, []*trace.Trace{t1, t2}, RunOptions{Budget: 5}, &out); err != nil {
		t.Fatal(err)
	}
	if len(m.seen) < 3 || !strings.Contains(m.seen[2][0].Content, "Owns a Lumen Drift vinyl.") {
		t.Fatal("a dry run must show what a real run would, or its page misleads")
	}
}
