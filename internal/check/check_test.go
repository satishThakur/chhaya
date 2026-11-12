package check

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/trace"
)

func msg(id int64, role, content string) trace.Message {
	return trace.Message{ID: id, Role: role, Content: content}
}

func call(id, name, args string) trace.Call {
	return trace.Call{ID: id, Name: name, Args: json.RawMessage(args)}
}

func tg(ms ...trace.Message) *trace.Trace {
	return &trace.Trace{ID: "t", Day: "2026-11-11", Source: "telegram", Messages: ms,
		Counters: trace.Counters{MemoryCallsBeforeFirstQuestion: -1}}
}

func fire(t *testing.T, name string, tr *trace.Trace) []Finding {
	t.Helper()
	for _, c := range All() {
		if c.Name == name {
			return c.Run(tr)
		}
	}
	t.Fatalf("no check named %s", name)
	return nil
}

func wantN(t *testing.T, name string, tr *trace.Trace, n int) []Finding {
	t.Helper()
	got := fire(t, name, tr)
	if len(got) != n {
		t.Fatalf("%s: %d finding(s), want %d: %+v", name, len(got), n, got)
	}
	return got
}

func TestUnmatchedGatewayCallFails(t *testing.T) {
	tr := tg()
	wantN(t, "integrity.unmatched", tr, 0)
	tr.Unmatched = []trace.GatewayEntry{{Seq: 7, Tool: "memory.get", Decision: "allow"}}
	wantN(t, "integrity.unmatched", tr, 1)
}

func TestOrphanCall(t *testing.T) {
	a := msg(1, "assistant", "")
	a.Calls = []trace.Call{call("c1", "x", `{}`)}
	wantN(t, "integrity.orphan_call", tg(a), 1)

	res := msg(2, "tool", "ok")
	res.ToolCallID = "c1"
	wantN(t, "integrity.orphan_call", tg(a, res), 0)

	// two inner calls wrapped in one envelope share the outer id and one result
	b := msg(3, "assistant", "")
	b.Calls = []trace.Call{call("c2", "x", `{}`), call("c2", "y", `{}`)}
	r2 := msg(4, "tool", "ok")
	r2.ToolCallID = "c2"
	wantN(t, "integrity.orphan_call", tg(b, r2), 0)
}

func TestLLMLink(t *testing.T) {
	tr := tg()
	wantN(t, "integrity.llm_link", tr, 0) // no dumps is not a finding
	tr.LLM = &trace.LLM{Calls: []trace.LLMCall{{N: 1, Reply: 5}, {N: 2}}, Problems: []string{"x.json: bad"}}
	got := wantN(t, "integrity.llm_link", tr, 2)
	if !strings.Contains(got[0].Detail, "call 2") {
		t.Errorf("must name the unlinked call: %+v", got)
	}
}

// Token-shaped strings are built at run time so this file contains none.
func fakeKeys() map[string]string {
	return map[string]string{
		"Anthropic key":      "sk-" + "ant-" + strings.Repeat("a1", 12),
		"API key (sk-)":      "sk-" + strings.Repeat("Z9", 12),
		"Langfuse key":       "pk-" + "lf-" + "12345678-abcd",
		"AWS access key id":  "AK" + "IA" + "ABCDEFGHIJKLMNOP",
		"Telegram bot token": "1234567890" + ":" + strings.Repeat("A", 35),
		"GitHub token":       "gh" + "p_" + strings.Repeat("a", 36),
		"private key block":  "-----BEGIN " + "RSA PRIVATE KEY-----",
	}
}

func TestSecretsFindsEachShapeAndNeverPrintsTheValue(t *testing.T) {
	for kind, val := range fakeKeys() {
		tr := tg(msg(9, "tool", "the output was "+val+" and more"))
		got := wantN(t, "secrets.in_trace", tr, 1)
		if !strings.Contains(got[0].Detail, kind) || got[0].MessageID != 9 {
			t.Errorf("%s: finding %+v", kind, got[0])
		}
		if strings.Contains(got[0].Detail, val) {
			t.Errorf("%s: the finding contains the secret", kind)
		}
	}
}

func TestSecretsLooksInArgsAuditAndRequests(t *testing.T) {
	k := fakeKeys()["Anthropic key"]
	a := msg(1, "assistant", "")
	a.Calls = []trace.Call{call("c", "terminal", `{"cmd":"curl -H `+k+`"}`)}
	wantN(t, "secrets.in_trace", tg(a), 1)

	g := msg(2, "tool", "ok")
	g.Gateway = &trace.GatewayEntry{Result: json.RawMessage(`"` + k + `"`)}
	wantN(t, "secrets.in_trace", tg(g), 1)

	tr := tg()
	tr.LLM = &trace.LLM{Systems: map[string]json.RawMessage{"h": json.RawMessage(`"` + k + `"`)}}
	wantN(t, "secrets.in_trace", tr, 1)
	tr.LLM = &trace.LLM{Calls: []trace.LLMCall{{N: 3, New: []json.RawMessage{json.RawMessage(`{"content":"` + k + `"}`)}}}}
	wantN(t, "secrets.in_trace", tr, 1)
}

func TestSecretsIgnoresLookAlikes(t *testing.T) {
	for _, s := range []string{
		"sk-short", "task-queue-name-with-long-text-1234567890", "call me on 9876543210:",
		"a time 10:30:45 and a ratio 12345678:99", "AKIA-not-a-key", "ghp_short", "pk-lf-",
	} {
		wantN(t, "secrets.in_trace", tg(msg(1, "user", s)), 0)
	}
}

func TestUnansweredHumanMessage(t *testing.T) {
	u := msg(1, "user", "hi")
	a := msg(2, "assistant", "hello")
	wantN(t, "reply.unanswered_human_message", tg(u, a), 0)

	// the model only asked for a tool and the session stopped: no text reply
	tool := msg(2, "assistant", "")
	tool.Calls = []trace.Call{call("c", "x", `{}`)}
	got := wantN(t, "reply.unanswered_human_message", tg(u, tool), 1)
	if got[0].MessageID != 1 {
		t.Errorf("must point at the unanswered message: %+v", got[0])
	}

	// the next human message arrives first
	wantN(t, "reply.unanswered_human_message", tg(u, msg(3, "user", "hello??"), msg(4, "assistant", "yes")), 1)
}

func TestInjectedMessagesAreNotTheOwner(t *testing.T) {
	for _, p := range []string{"[Cron delivery: Digest]\nbody", "[ASYNC DELEGATION COMPLETE — x]", "[System note: x]", "[IMPORTANT: skill]"} {
		wantN(t, "reply.unanswered_human_message", tg(msg(1, "user", p)), 0)
	}
	// an injected message between the owner's message and the reply does not hide the reply
	wantN(t, "reply.unanswered_human_message",
		tg(msg(1, "user", "hi"), msg(2, "user", "[Cron delivery: x]"), msg(3, "assistant", "hello")), 0)
	// and does not count as the next message of the owner
	wantN(t, "reply.unanswered_human_message",
		tg(msg(1, "user", "hi"), msg(2, "user", "[Cron delivery: x]")), 1)
}

func TestLastMessageGetsAGraceWindow(t *testing.T) {
	tr := tg(msg(1, "user", "hi"))
	tr.LastMessage = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	tr.ExportedAt = tr.LastMessage.Add(2 * time.Minute)
	wantN(t, "reply.unanswered_human_message", tr, 0) // may simply not be answered yet
	tr.ExportedAt = tr.LastMessage.Add(30 * time.Minute)
	wantN(t, "reply.unanswered_human_message", tr, 1)
}

func TestToolErrorsAreParsedNotMatchedAsText(t *testing.T) {
	wrap := func(s string) string {
		return "<untrusted_tool_result source=\"x\">\nThe following content was retrieved from an external source.\n\n" + s + "\n</untrusted_tool_result>"
	}
	cases := []struct {
		name, content string
		want          int
	}{
		{"null error is not an error", `{"output": "", "exit_code": 0, "error": null}`, 0},
		{"empty error is not an error", `{"error": ""}`, 0},
		{"plain text mentioning error", `an error occurred in my story, "error": appears here`, 0},
		{"error string", `{"success": false, "error": "Write denied: outside root"}`, 1},
		{"wrapped gateway error", wrap(`{"error": "forbidden by policy"}`), 1},
		{"status error", `{"status": "error", "output": "x"}`, 1},
		{"success false alone", `{"success": false}`, 1},
		{"success true", `{"success": true, "output": "fine"}`, 0},
		{"error that is an object", `{"error": {"code": 3}}`, 1},
	}
	for _, c := range cases {
		m := msg(1, "tool", c.content)
		m.ToolName = "terminal"
		if got := fire(t, "tool.error", tg(m)); len(got) != c.want {
			t.Errorf("%s: %d finding(s), want %d: %+v", c.name, len(got), c.want, got)
		}
	}
}

func TestMemoryMissIsItsOwnFindingAndNotAToolError(t *testing.T) {
	miss := msg(1, "tool", "<untrusted_tool_result source=\"mcp__chhaya__memory_get\">\nthe following\n\n{\"error\": \"no memory document at \\\"taste/reading\\\"\"}\n</untrusted_tool_result>")
	miss.ToolName = "mcp__chhaya__memory_get"
	got := wantN(t, "memory.miss", tg(miss), 1)
	if !strings.Contains(got[0].Detail, "taste/reading") {
		t.Errorf("must name the guessed path: %+v", got[0])
	}
	wantN(t, "tool.error", tg(miss), 0)

	other := msg(2, "tool", `{"error": "something else"}`)
	other.ToolName = "mcp__chhaya__memory_get"
	wantN(t, "memory.miss", tg(other), 0)
	wantN(t, "tool.error", tg(other), 1)

	wrongTool := msg(3, "tool", `{"error": "no memory document at \"x\""}`)
	wrongTool.ToolName = "read_file"
	wantN(t, "memory.miss", tg(wrongTool), 0)

	ok := msg(4, "tool", `{"result": "{\"trust\":\"trusted\"}"}`)
	ok.ToolName = "mcp__chhaya__memory_get"
	wantN(t, "memory.miss", tg(ok), 0)
}

func TestRepeatedIdenticalCalls(t *testing.T) {
	mk := func(id int64, args string) trace.Message {
		m := msg(id, "assistant", "")
		m.Calls = []trace.Call{call("c", "web_search", args)}
		return m
	}
	wantN(t, "loop.repeat_call", tg(mk(1, `{"q":"a"}`), mk(2, `{"q":"a"}`)), 0)
	got := wantN(t, "loop.repeat_call", tg(mk(1, `{"q":"a"}`), mk(2, `{"q":"a"}`), mk(3, `{"q":"a"}`)), 1)
	if got[0].MessageID != 1 || !strings.Contains(got[0].Detail, "3 times") {
		t.Errorf("%+v", got[0])
	}
	// different arguments are not a repeat; key order does not hide one
	wantN(t, "loop.repeat_call", tg(mk(1, `{"q":"a"}`), mk(2, `{"q":"b"}`), mk(3, `{"q":"c"}`)), 0)
	wantN(t, "loop.repeat_call", tg(mk(1, `{"a":1,"b":2}`), mk(2, `{"b":2,"a":1}`), mk(3, `{"a": 1, "b": 2}`)), 1)
}

func TestToolSurfaceListsOnlyNonGatewayTools(t *testing.T) {
	m := msg(1, "assistant", "")
	m.Calls = []trace.Call{
		call("1", "mcp__chhaya__memory_get", `{}`), call("2", "tool_call", `{}`), call("3", "tool_search", `{}`),
		call("4", "terminal", `{}`), call("5", "terminal", `{}`), call("6", "execute_code", `{}`),
	}
	got := wantN(t, "tools.surface", tg(m), 1)
	if got[0].Detail != "non-gateway tools: execute_code ×1, terminal ×2" {
		t.Errorf("detail = %q", got[0].Detail)
	}
	only := msg(2, "assistant", "")
	only.Calls = []trace.Call{call("1", "mcp__chhaya__memory_list", `{}`)}
	wantN(t, "tools.surface", tg(only), 0)
}

func TestAskBeforeMemory(t *testing.T) {
	tr := tg()
	tr.Counters = trace.Counters{Questions: 1, MemoryCallsBeforeFirstQuestion: 0, FirstQuestionMessageID: 12}
	got := wantN(t, "ask.before_memory", tr, 1)
	if got[0].MessageID != 12 {
		t.Errorf("%+v", got[0])
	}
	tr.Counters.MemoryCallsBeforeFirstQuestion = 2
	wantN(t, "ask.before_memory", tr, 0)
	tr.Counters = trace.Counters{MemoryCallsBeforeFirstQuestion: -1}
	wantN(t, "ask.before_memory", tr, 0)
}

func TestRunAppliesChecksBySourceAndOwnsTheLevel(t *testing.T) {
	cron := &trace.Trace{ID: "c", Source: "cron", Messages: []trace.Message{msg(1, "user", "go")}}
	r := Run(cron, All())
	for _, n := range r.Ran {
		if n == "reply.unanswered_human_message" || n == "memory.miss" || n == "ask.before_memory" {
			t.Errorf("%s must not run on a cron trace", n)
		}
	}
	if len(r.Findings) != 0 {
		t.Errorf("findings: %+v", r.Findings)
	}

	// a check's own findings cannot carry a different level than the check
	c := Check{Name: "x", Level: Info, Run: func(*trace.Trace) []Finding {
		return []Finding{{Level: Fail, Detail: "tries to escalate"}}
	}}
	if got := Run(cron, []Check{c}).Findings[0]; got.Level != Info || got.Check != "x" {
		t.Errorf("level must come from the check: %+v", got)
	}
}

func TestSummarizeAndFailures(t *testing.T) {
	checks := []Check{{Name: "a", Level: Fail}, {Name: "b", Level: Flag}}
	rs := []TraceResult{
		{ID: "1", Ran: []string{"a", "b"}, Findings: []Finding{{Check: "a", Level: Fail}, {Check: "a", Level: Fail}}},
		{ID: "2", Ran: []string{"a"}},
	}
	s := Summarize(rs, checks)
	if s[0].Ran != 2 || s[0].Fired != 1 || s[0].Findings != 2 || s[1].Ran != 1 || s[1].Fired != 0 {
		t.Errorf("%+v", s)
	}
	if Failures(rs) != 2 {
		t.Errorf("failures = %d", Failures(rs))
	}
}

func TestNotResetFlagsAConversationThatBeganWithYouAfterALongSilence(t *testing.T) {
	later := tg(msg(5, "user", "hello again"), msg(6, "assistant", "hi"))
	later.SessionID, later.Segment, later.Segments, later.GapBeforeMinutes = "s1", 2, 2, 240
	got := wantN(t, "session.not_reset", later, 1)
	if got[0].MessageID != 5 || !strings.Contains(got[0].Detail, "240 minutes") {
		t.Errorf("%+v", got[0])
	}

	// the first conversation of a session has no silence before it
	wantN(t, "session.not_reset", tg(msg(1, "user", "hi")), 0)

	// a conversation that begins with a cron delivery is not the owner continuing a stale session
	cron := tg(msg(5, "user", "[Cron delivery: Digest]\nbody"), msg(6, "assistant", "ok"))
	cron.SessionID, cron.Segment, cron.Segments, cron.GapBeforeMinutes = "s1", 2, 2, 240
	wantN(t, "session.not_reset", cron, 0)
}
