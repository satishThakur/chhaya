package trace

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var segT0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) // 15:30 IST

func at(min float64) time.Time { return segT0.Add(time.Duration(min * float64(time.Minute))) }

func sess(ms ...Message) *Trace {
	return &Trace{Schema: 2, ID: "s1", Day: Day(ms[0].Time), Source: "telegram", Started: ms[0].Time,
		LastMessage: ms[len(ms)-1].Time, Messages: ms, InputTokens: 900, OutputTokens: 90, CostUSD: 1.5,
		SystemPromptHash: "h", Title: "T"}
}

func m(id int64, role string, min float64, content string) Message {
	return Message{ID: id, Role: role, Time: at(min), Content: content}
}

func TestSplitCutsOnlyWhereTheSilenceExceedsTheGap(t *testing.T) {
	tr := sess(m(1, "user", 0, "a"), m(2, "assistant", 1, "b"), m(3, "user", 62, "c"), m(4, "assistant", 63, "d"))
	segs := Split(tr, time.Hour)
	if len(segs) != 2 {
		t.Fatalf("%d segments, want 2", len(segs))
	}
	if segs[0].ID != "s1" || segs[1].ID != "s1.2" {
		t.Errorf("ids = %s, %s; the first must keep the session id", segs[0].ID, segs[1].ID)
	}
	if len(segs[0].Messages) != 2 || len(segs[1].Messages) != 2 || segs[1].Messages[0].ID != 3 {
		t.Errorf("messages not partitioned: %d / %d", len(segs[0].Messages), len(segs[1].Messages))
	}
	if segs[1].SessionID != "s1" || segs[1].Segment != 2 || segs[1].Segments != 2 || segs[1].GapBeforeMinutes != 61 {
		t.Errorf("segment fields: %+v", segs[1])
	}
	if segs[0].Counters.UserMessages != 1 || segs[1].Counters.UserMessages != 1 || segs[1].MessageCount != 2 {
		t.Errorf("counters must be per conversation: %+v %+v", segs[0].Counters, segs[1].Counters)
	}
	if segs[1].Started != at(62) || segs[1].LastMessage != at(63) {
		t.Errorf("window: %v .. %v", segs[1].Started, segs[1].LastMessage)
	}
}

func TestSplitGapIsStrictAndZeroDisablesIt(t *testing.T) {
	exact := sess(m(1, "user", 0, "a"), m(2, "user", 60, "b"))
	if n := len(Split(exact, time.Hour)); n != 1 {
		t.Errorf("exactly the gap must not split, got %d", n)
	}
	over := sess(m(1, "user", 0, "a"), Message{ID: 2, Role: "user", Time: at(60).Add(time.Second)})
	if n := len(Split(over, time.Hour)); n != 2 {
		t.Errorf("one second over must split, got %d", n)
	}
	if n := len(Split(sess(m(1, "user", 0, "a"), m(2, "user", 5000, "b")), 0)); n != 1 {
		t.Errorf("gap 0 must never split, got %d", n)
	}
}

func TestAnUnsplitSessionIsReturnedUntouched(t *testing.T) {
	tr := sess(m(1, "user", 0, "a"), m(2, "assistant", 30, "b"))
	segs := Split(tr, time.Hour)
	if len(segs) != 1 || segs[0] != tr || tr.Segment != 0 || tr.SessionID != "" {
		t.Errorf("an unsplit trace must be exactly as before: %+v", segs[0])
	}
}

func TestSessionTotalsStayOnTheFirstConversationAndLaterOnesFileUnderTheirOwnDay(t *testing.T) {
	// 23:00 IST, then the next afternoon
	ms := []Message{
		{ID: 1, Role: "user", Time: time.Date(2026, 11, 9, 17, 30, 0, 0, time.UTC)},
		{ID: 2, Role: "user", Time: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)},
	}
	tr := sess(ms...)
	segs := Split(tr, time.Hour)
	if segs[0].InputTokens != 900 || segs[0].CostUSD != 1.5 || segs[1].InputTokens != 0 || segs[1].CostUSD != 0 {
		t.Errorf("session totals must not be duplicated: %+v / %+v", segs[0], segs[1])
	}
	if segs[0].Day != "2026-11-09" || segs[1].Day != "2026-11-10" {
		t.Errorf("days = %s, %s (IST)", segs[0].Day, segs[1].Day)
	}
	if segs[1].Title != "T" || segs[1].Source != "telegram" || segs[1].SystemPromptHash != "h" {
		t.Errorf("header fields must carry over: %+v", segs[1])
	}
}

func TestGatewayEntriesGoToTheConversationTheyBelongTo(t *testing.T) {
	tr := sess(m(1, "user", 0, "a"), m(2, "assistant", 1, "b"), m(3, "user", 120, "c"), m(4, "assistant", 121, "d"))
	tr.Unmatched = []GatewayEntry{
		{Seq: 1, TS: at(0.5)},                       // inside the first
		{Seq: 2, TS: at(120).Add(-3 * time.Second)}, // just before the second begins: its own turn
		{Seq: 3, TS: at(121.5)},                     // inside the second
	}
	tr.Events = []GatewayEntry{
		{Seq: 4, Actor: "approver", TS: at(20)},  // an approval tap in the silence belongs to what came before
		{Seq: 5, Actor: "approver", TS: at(125)}, // after the second
	}
	segs := Split(tr, time.Hour)
	ids := func(es []GatewayEntry) string {
		var s []string
		for _, e := range es {
			s = append(s, string(rune('0'+e.Seq)))
		}
		return strings.Join(s, ",")
	}
	if ids(segs[0].Unmatched) != "1" || ids(segs[1].Unmatched) != "2,3" {
		t.Errorf("unmatched: %q | %q", ids(segs[0].Unmatched), ids(segs[1].Unmatched))
	}
	if ids(segs[0].Events) != "4" || ids(segs[1].Events) != "5" {
		t.Errorf("events: %q | %q", ids(segs[0].Events), ids(segs[1].Events))
	}
}

func rawj(s string) json.RawMessage { return json.RawMessage(s) }

func TestRequestsFollowTheirRepliesAndALaterConversationKeepsItsCarriedHistory(t *testing.T) {
	tr := sess(
		m(10, "user", 0, "a"), m(11, "assistant", 1, "b"),
		m(12, "user", 200, "c"), m(13, "assistant", 201, "d"),
	)
	u1, a1, u2 := rawj(`{"role":"user","content":"a"}`), rawj(`{"role":"assistant","content":"b"}`), rawj(`{"role":"user","content":"c"}`)
	tr.LLM = &LLM{
		Systems:  map[string]json.RawMessage{"S1": rawj(`"sys"`), "S2": rawj(`"other"`)},
		ToolSets: map[string]json.RawMessage{"T1": rawj(`[]`)},
		Calls: []LLMCall{
			{N: 1, System: "S1", Tools: "T1", Total: 1, New: []json.RawMessage{u1}, Reply: 11},
			{N: 2, System: "S2", Tools: "T1", Total: 3, New: []json.RawMessage{a1, u2}, Reply: 13},
		},
		Problems: []string{"x.json: bad"}, OtherDumps: 2,
	}
	segs := Split(tr, time.Hour)
	a, b := segs[0].LLM, segs[1].LLM
	if len(a.Calls) != 1 || a.Calls[0].N != 1 || len(b.Calls) != 1 || b.Calls[0].N != 2 {
		t.Fatalf("calls: %+v / %+v", a.Calls, b.Calls)
	}
	if _, ok := a.Systems["S2"]; ok || len(b.Systems) != 1 || b.Systems["S2"] == nil {
		t.Errorf("each conversation keeps only the prompts it used: %v | %v", a.Systems, b.Systems)
	}
	c := b.Calls[0]
	if !c.Reset || len(c.New) != 3 || c.Carried != 2 {
		t.Errorf("the first request of a later conversation is stored in full, with what was carried over: %+v", c)
	}
	if a.OtherDumps != 2 || len(a.Problems) != 1 || b.OtherDumps != 0 {
		t.Errorf("dump problems belong to the first conversation only: %+v %+v", a, b)
	}
}

func TestACallWithNoReplyFollowsTheCallBeforeIt(t *testing.T) {
	tr := sess(m(10, "user", 0, "a"), m(11, "assistant", 1, "b"), m(12, "user", 200, "c"), m(13, "assistant", 201, "d"))
	tr.LLM = &LLM{Systems: map[string]json.RawMessage{"S": rawj(`"s"`)}, ToolSets: map[string]json.RawMessage{"T": rawj(`[]`)},
		Calls: []LLMCall{
			{N: 1, System: "S", Tools: "T", Total: 1, New: []json.RawMessage{rawj(`{}`)}}, // no reply, before any linked call
			{N: 2, System: "S", Tools: "T", Total: 2, New: []json.RawMessage{rawj(`{}`)}, Reply: 11},
			{N: 3, System: "S", Tools: "T", Total: 3, New: []json.RawMessage{rawj(`{}`)}, Reply: 13},
			{N: 4, System: "S", Tools: "T", Total: 4, New: []json.RawMessage{rawj(`{}`)}}, // no reply, after a linked call
		}}
	segs := Split(tr, time.Hour)
	if len(segs[0].LLM.Calls) != 2 || len(segs[1].LLM.Calls) != 2 {
		t.Fatalf("calls per conversation: %d / %d", len(segs[0].LLM.Calls), len(segs[1].LLM.Calls))
	}
	if segs[0].LLM.Calls[0].N != 1 || segs[1].LLM.Calls[1].N != 4 {
		t.Errorf("an unlinked call must stay with its neighbours")
	}
}

func TestExportWritesEachConversationUnderItsOwnDay(t *testing.T) {
	data := `
INSERT INTO sessions VALUES ('s_gap','telegram','42','m','T',NULL,1794203279,900,90,0.0,1.5,'h');
INSERT INTO messages (session_id,role,content,timestamp) VALUES ('s_gap','user','first',1794203279.0);
INSERT INTO messages (session_id,role,content,finish_reason,timestamp) VALUES ('s_gap','assistant','one','stop',1794203300.0);
INSERT INTO messages (session_id,role,content,timestamp) VALUES ('s_gap','user','much later',1794303279.0);
INSERT INTO messages (session_id,role,content,finish_reason,timestamp) VALUES ('s_gap','assistant','two','stop',1794303300.0);
`
	home := fixtureHome(t, schema, data, "")
	out := t.TempDir()
	res, err := Export(ExportOptions{Home: home, OutDir: out, Since: 0, Gap: time.Hour, Now: time.Unix(1794400000, 0)})
	must(t, err)
	if len(res.Written) != 2 {
		t.Fatalf("written %v", res.Written)
	}
	first := readTrace(t, filepath.Join(out, "days", "2026-11-09", "trace-s_gap.json"))
	if first.Segment != 1 || first.Segments != 2 || first.MessageCount != 2 || first.InputTokens != 900 {
		t.Errorf("first: %+v", first)
	}
	later := readTrace(t, filepath.Join(out, "days", "2026-11-10", "trace-s_gap.2.json"))
	if later.Segment != 2 || later.SessionID != "s_gap" || later.MessageCount != 2 || later.InputTokens != 0 || later.GapBeforeMinutes < 60 {
		t.Errorf("later: %+v", later)
	}
	// without -gap the same session is one trace, as before
	out2 := t.TempDir()
	res2, err := Export(ExportOptions{Home: home, OutDir: out2, Since: 0, Now: time.Unix(1794400000, 0)})
	must(t, err)
	if len(res2.Written) != 1 {
		t.Errorf("no gap must not split: %v", res2.Written)
	}
}
