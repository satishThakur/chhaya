package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDump(t *testing.T, dir, name, sid, ts, reason string, sys string, tools string, msgs ...string) {
	t.Helper()
	all := append([]string{fmt.Sprintf(`{"role":"system","content":%q}`, sys)}, msgs...)
	body := fmt.Sprintf(`{"timestamp":%q,"session_id":%q,"reason":%q,"request":{"body":{"model":"m1","reasoning_effort":"low","tools":%s,"messages":[%s]}}}`,
		ts, sid, reason, tools, strings.Join(all, ","))
	if err := os.WriteFile(filepath.Join(dir, "request_dump_"+name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	uMsg  = `{"role":"user","content":"hi"}`
	a1Msg = `{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}`
	t1Msg = `{"role":"tool","tool_call_id":"c1","content":"result"}`
	a2Msg = `{"role":"assistant","content":"done"}`
)

func sessionTrace() *Trace {
	return &Trace{ID: "s1", Messages: []Message{
		{ID: 10, Role: "user", Content: "hi"},
		{ID: 11, Role: "assistant", Calls: []Call{{ID: "c1", Name: "x"}}},
		{ID: 12, Role: "tool", ToolCallID: "c1"},
		{ID: 13, Role: "assistant", Content: "done"},
	}}
}

func TestReadLLMStoresPromptOnceAndOnlyTheNewMessages(t *testing.T) {
	d := t.TempDir()
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "preflight", "SYS", `[{"name":"t"}]`, uMsg)
	writeDump(t, d, "s1_2", "s1", "2026-11-11T10:00:02", "preflight", "SYS", `[{"name":"t"}]`, uMsg, a1Msg, t1Msg)
	writeDump(t, d, "s1_3", "s1", "2026-11-11T10:00:03", "preflight", "SYS", `[{"name":"t"}]`, uMsg, a1Msg, t1Msg, a2Msg, `{"role":"user","content":"more"}`)
	l := ReadLLM(d, sessionTrace())
	if l == nil || len(l.Calls) != 3 {
		t.Fatalf("want 3 calls, got %+v", l)
	}
	if len(l.Systems) != 1 || len(l.ToolSets) != 1 {
		t.Errorf("prompt and tools must be stored once: %d systems, %d tool sets", len(l.Systems), len(l.ToolSets))
	}
	if got := []int{len(l.Calls[0].New), len(l.Calls[1].New), len(l.Calls[2].New)}; got[0] != 1 || got[1] != 2 || got[2] != 2 {
		t.Errorf("new messages per call = %v, want [1 2 2]", got)
	}
	if l.Calls[2].Total != 5 || l.Calls[0].Model != "m1" || l.Calls[0].ReasoningEffort != "low" {
		t.Errorf("call fields: %+v", l.Calls[2])
	}
	if l.Calls[0].Reply != 11 || l.Calls[1].Reply != 13 {
		t.Errorf("replies = %d, %d; want 11, 13 (matched by tool-call id / text)", l.Calls[0].Reply, l.Calls[1].Reply)
	}
}

func TestReadLLMFoldsRetriesAndFlagsRewrittenHistory(t *testing.T) {
	d := t.TempDir()
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "preflight", "SYS", `[]`, uMsg)
	writeDump(t, d, "s1_2", "s1", "2026-11-11T10:00:02", "preflight", "SYS", `[]`, uMsg) // retry
	writeDump(t, d, "s1_3", "s1", "2026-11-11T10:00:03", "preflight", "SYS2", `[]`, `{"role":"user","content":"[compacted summary]"}`)
	l := ReadLLM(d, sessionTrace())
	if len(l.Calls) != 2 {
		t.Fatalf("the retry must fold into the first call, got %d calls", len(l.Calls))
	}
	if l.Calls[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", l.Calls[0].Attempts)
	}
	if !l.Calls[1].Reset || len(l.Calls[1].New) != 1 {
		t.Errorf("a request that is not an extension must be marked Reset with all its messages: %+v", l.Calls[1])
	}
	if len(l.Systems) != 2 {
		t.Errorf("a changed system prompt must be kept as a second variant, got %d", len(l.Systems))
	}
}

func TestReadLLMIgnoresErrorDumpsAndOtherSessions(t *testing.T) {
	d := t.TempDir()
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "non_retryable_client_error", "SYS", `[]`, uMsg)
	// "s1" is a prefix of "s1_extra": the glob matches both, the session_id field decides.
	writeDump(t, d, "s1_extra_1", "s1_extra", "2026-11-11T10:00:02", "preflight", "SYS", `[]`, uMsg)
	l := ReadLLM(d, sessionTrace())
	if l == nil || len(l.Calls) != 0 || l.OtherDumps != 1 {
		t.Fatalf("want no calls and one error dump counted, got %+v", l)
	}
}

func TestReadLLMReportsACorruptDumpAndNothingElse(t *testing.T) {
	d := t.TempDir()
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "preflight", "SYS", `[]`, uMsg)
	os.WriteFile(filepath.Join(d, "request_dump_s1_2.json"), []byte("{not json"), 0o600)
	l := ReadLLM(d, sessionTrace())
	if len(l.Calls) != 1 || len(l.Problems) != 1 || !strings.Contains(l.Problems[0], "request_dump_s1_2.json") {
		t.Fatalf("the good dump must survive and the bad one be named: %+v", l)
	}
}

func TestReadLLMNilWithoutDumps(t *testing.T) {
	if l := ReadLLM(t.TempDir(), sessionTrace()); l != nil {
		t.Fatalf("no dumps must give nil, got %+v", l)
	}
	if l := ReadLLM(filepath.Join(t.TempDir(), "absent"), sessionTrace()); l != nil {
		t.Fatalf("a missing directory must give nil, got %+v", l)
	}
	if l := ReadLLM(t.TempDir(), &Trace{ID: "../x"}); l != nil {
		t.Fatal("an unsafe id must never reach a glob")
	}
}

func TestReplyIsNotLinkedWhenNothingMatches(t *testing.T) {
	d := t.TempDir()
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "preflight", "SYS", `[]`, uMsg)
	writeDump(t, d, "s1_2", "s1", "2026-11-11T10:00:02", "preflight", "SYS", `[]`, uMsg, `{"role":"assistant","content":"something state.db never had"}`, t1Msg)
	l := ReadLLM(d, sessionTrace())
	if l.Calls[0].Reply != 0 {
		t.Errorf("call 1 reply = %d; a reply that matches no stored message must stay unlinked", l.Calls[0].Reply)
	}
}

func TestExportCarriesTheRequestsIntoTheTrace(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	dir := filepath.Join(home, "hermes", "sessions")
	os.MkdirAll(dir, 0o700)
	writeDump(t, dir, "s_recent_1", "s_recent", "2026-11-09T10:00:01", "preflight", "SECRET-SYSTEM-PROMPT", `[{"name":"tool_a"}]`, uMsg)
	out := t.TempDir()
	res, err := Export(ExportOptions{Home: home, OutDir: out, Since: 0, Now: time.Unix(1794203400, 0)})
	must(t, err)
	var tr *Trace
	for _, p := range res.Written {
		if strings.HasSuffix(p, "trace-s_recent.json") {
			v := readTrace(t, p)
			tr = &v
		}
	}
	if tr == nil || tr.LLM == nil || len(tr.LLM.Calls) != 1 {
		t.Fatalf("trace must carry the dumped call: %+v", tr)
	}
	b, _ := json.Marshal(tr.LLM.Systems)
	if !strings.Contains(string(b), "SECRET-SYSTEM-PROMPT") {
		t.Error("the system prompt from the dump must be in the trace")
	}
	if tr.Schema != 2 {
		t.Errorf("schema = %d", tr.Schema)
	}
}

func TestTwoIdenticalRepliesLinkToTwoDifferentMessages(t *testing.T) {
	d := t.TempDir()
	ok := `{"role":"assistant","content":"ok"}`
	u2 := `{"role":"user","content":"again"}`
	writeDump(t, d, "s1_1", "s1", "2026-11-11T10:00:01", "preflight", "SYS", `[]`, uMsg)
	writeDump(t, d, "s1_2", "s1", "2026-11-11T10:00:02", "preflight", "SYS", `[]`, uMsg, ok, u2)
	writeDump(t, d, "s1_3", "s1", "2026-11-11T10:00:03", "preflight", "SYS", `[]`, uMsg, ok, u2, ok, `{"role":"user","content":"x"}`)
	tr := &Trace{ID: "s1", Messages: []Message{
		{ID: 1, Role: "user"}, {ID: 2, Role: "assistant", Content: "ok"},
		{ID: 3, Role: "user"}, {ID: 4, Role: "assistant", Content: "ok"},
	}}
	l := ReadLLM(d, tr)
	if l.Calls[0].Reply != 2 || l.Calls[1].Reply != 4 {
		t.Fatalf("replies = %d, %d; want 2, 4 (each message claimed once)", l.Calls[0].Reply, l.Calls[1].Reply)
	}
}
