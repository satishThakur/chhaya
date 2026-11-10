package trace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGatewayName(t *testing.T) {
	for in, want := range map[string]string{
		"mcp__chhaya__memory_get":            "memory.get",
		"mcp__chhaya__memory_propose_update": "memory.propose_update",
		"mcp__chhaya__ping":                  "ping",
		"tool_search":                        "",
		"mcp__other__memory_get":             "",
		"mcp__chhaya__":                      "",
	} {
		if got := GatewayName(in); got != want {
			t.Errorf("GatewayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDayIsISTNotUTC(t *testing.T) {
	// 20:00 UTC on the 9th is 01:30 on the 10th in IST.
	if got := Day(time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)); got != "2026-11-10" {
		t.Fatalf("Day = %s, want 2026-11-10", got)
	}
	if got := Day(time.Date(2026, 11, 9, 18, 29, 0, 0, time.UTC)); got != "2026-11-09" {
		t.Fatalf("Day = %s, want 2026-11-09", got)
	}
}

func TestParseCallsUnwrapsEnvelope(t *testing.T) {
	raw := `[{"id":"c1","type":"function","function":{"name":"tool_call","arguments":"{\"calls\":[{\"name\":\"mcp__chhaya__memory_get\",\"arguments\":{\"path\":\"location\"}},{\"name\":\"mcp__chhaya__memory_list\",\"arguments\":{}}]}"}},
	{"id":"c2","function":{"name":"tool_search","arguments":"{\"queries\":[\"x\"]}"}}]`
	calls := ParseCalls(raw)
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
	}
	if calls[0].Name != "mcp__chhaya__memory_get" || calls[0].Via != "tool_call" || calls[0].ID != "c1" {
		t.Errorf("first call = %+v", calls[0])
	}
	if string(calls[0].Args) != `{"path":"location"}` {
		t.Errorf("args = %s", calls[0].Args)
	}
	if calls[2].Name != "tool_search" || calls[2].Via != "" {
		t.Errorf("plain call = %+v", calls[2])
	}
}

func TestParseCallsNeverDropsWhatItCannotRead(t *testing.T) {
	if c := ParseCalls(`{not json`); len(c) != 1 || c[0].Name != "unparsed" {
		t.Fatalf("malformed input must surface as 'unparsed', got %+v", c)
	}
	c := ParseCalls(`[{"id":"c","function":{"name":"x","arguments":"not json at all"}}]`)
	if len(c) != 1 || !json.Valid(c[0].Args) {
		t.Fatalf("non-JSON arguments must be kept as a JSON string: %+v", c)
	}
	if ParseCalls("") != nil || ParseCalls("null") != nil {
		t.Fatal("empty input has no calls")
	}
}

var t0 = time.Date(2026, 11, 9, 5, 48, 0, 0, time.UTC)

func entry(seq int64, actor, tool string, off time.Duration) GatewayEntry {
	return GatewayEntry{Seq: seq, TS: t0.Add(off), Actor: actor, Tool: tool, Decision: "allow"}
}

func toolMsg(id int64, name string, off time.Duration) Message {
	return Message{ID: id, Role: "tool", ToolName: name, Time: t0.Add(off)}
}

func TestMatcherPairsByToolAndNearestTime(t *testing.T) {
	m := NewMatcher([]GatewayEntry{
		entry(1, "hermes", "memory.get", 0),
		entry(2, "hermes", "memory.get", 10*time.Second),
		entry(3, "hermes", "memory.list", 10*time.Second),
	})
	msgs := []Message{
		toolMsg(1, "mcp__chhaya__memory_get", 100*time.Millisecond),
		toolMsg(2, "mcp__chhaya__memory_get", 10*time.Second+50*time.Millisecond),
		toolMsg(3, "mcp__chhaya__memory_list", 10*time.Second),
		toolMsg(4, "tool_search", 0),
	}
	m.Attach(msgs)
	for i, want := range []int64{1, 2, 3, 0} {
		got := int64(0)
		if msgs[i].Gateway != nil {
			got = msgs[i].Gateway.Seq
		}
		if got != want {
			t.Errorf("message %d matched seq %d, want %d", i+1, got, want)
		}
	}
}

func TestMatcherNeverClaimsAnEntryTwice(t *testing.T) {
	m := NewMatcher([]GatewayEntry{entry(1, "hermes", "memory.get", 0)})
	a := []Message{toolMsg(1, "mcp__chhaya__memory_get", 0)}
	b := []Message{toolMsg(2, "mcp__chhaya__memory_get", time.Second)} // another session, same moment
	m.Attach(a)
	m.Attach(b)
	if a[0].Gateway == nil || b[0].Gateway != nil {
		t.Fatalf("one audit entry must belong to exactly one message: a=%v b=%v", a[0].Gateway, b[0].Gateway)
	}
}

func TestMatcherIgnoresOtherActorsAndFarEntries(t *testing.T) {
	m := NewMatcher([]GatewayEntry{
		entry(1, "approver", "memory.get", 0),         // wrong actor
		entry(2, "hermes", "memory.get", time.Minute), // too far
	})
	msgs := []Message{toolMsg(1, "mcp__chhaya__memory_get", 0)}
	m.Attach(msgs)
	if msgs[0].Gateway != nil {
		t.Fatalf("matched %+v; a wrong guess here would put the wrong decision on a trace", msgs[0].Gateway)
	}
}

func TestResidueFlagsUnmatchedAndListsApproverEvents(t *testing.T) {
	m := NewMatcher([]GatewayEntry{
		entry(1, "hermes", "memory.get", 0),
		entry(2, "hermes", "ping", 20*time.Second),           // nobody claimed it
		entry(3, "approver", "memory.apply", 10*time.Minute), // a late tap on the approval card
		entry(4, "hermes", "ping", 10*time.Hour),             // outside the window
		entry(5, "approver", "memory.apply", 10*time.Hour),   // an approval for something else
	})
	msgs := []Message{toolMsg(1, "mcp__chhaya__memory_get", 0)}
	m.Attach(msgs)
	un, ev := m.Residue(t0, t0.Add(time.Minute))
	if len(un) != 1 || un[0].Seq != 2 {
		t.Errorf("unmatched = %+v, want only seq 2", un)
	}
	if len(ev) != 1 || ev[0].Seq != 3 {
		t.Errorf("events = %+v, want only seq 3 (late tap in, distant approval out)", ev)
	}
}

func TestCountQuestionsAndMemoryCallsBeforeThem(t *testing.T) {
	get := Call{Name: "mcp__chhaya__memory_get"}
	// Asked first, looked never.
	c := Count([]Message{
		{ID: 1, Role: "user", Content: "coffee near home"},
		{ID: 2, Role: "assistant", Content: "What is your address?"},
	})
	if c.Questions != 1 || c.MemoryCallsBeforeFirstQuestion != 0 || c.FirstQuestionMessageID != 2 {
		t.Errorf("asked-without-looking: %+v", c)
	}
	// Looked twice, then asked.
	c = Count([]Message{
		{ID: 1, Role: "user", Content: "x"},
		{ID: 2, Role: "assistant", Calls: []Call{get, get}},
		{ID: 3, Role: "assistant", Content: "Which one?"},
	})
	if c.MemoryCallsBeforeFirstQuestion != 2 || c.GatewayCalls["memory.get"] != 2 {
		t.Errorf("looked-then-asked: %+v", c)
	}
	// Never asked.
	c = Count([]Message{{ID: 1, Role: "user", Content: "hi"}, {ID: 2, Role: "assistant", Content: "Hello."}})
	if c.MemoryCallsBeforeFirstQuestion != -1 || c.Questions != 0 {
		t.Errorf("never-asked: %+v", c)
	}
	// A "?" in a message that also makes a call is not a question turn.
	c = Count([]Message{{ID: 1, Role: "assistant", Content: "Checking, ok?", Calls: []Call{get}}})
	if c.Questions != 0 {
		t.Errorf("a reply that makes a call is not a question turn: %+v", c)
	}
}

func TestParseAuditRejectsCorruptLines(t *testing.T) {
	if _, err := parseAudit(strings.NewReader(`{"seq":1,"actor":"hermes"}` + "\n" + `{broken`)); err == nil {
		t.Fatal("a corrupt audit line must fail the export, not be skipped")
	}
	got, err := parseAudit(strings.NewReader("\n" + `{"seq":1,"ts":"2026-11-09T05:48:03.946466352Z","actor":"hermes","tool":"memory.list","tier":0,"decision":"allow"}` + "\n\n"))
	if err != nil || len(got) != 1 || got[0].Tool != "memory.list" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
