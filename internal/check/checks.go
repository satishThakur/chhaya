package check

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/trace"
)

// All is the default set, in the order it is reported.
func All() []Check {
	return []Check{
		{Name: "integrity.unmatched", Level: Fail, Run: unmatched,
			Doc: "Every gateway call Hermes made is tied to a tool message. A miss means the trace is wrong, not the agent."},
		{Name: "integrity.orphan_call", Level: Fail, Run: orphanCalls,
			Doc: "Every tool call the model made has a result message."},
		{Name: "integrity.llm_link", Level: Flag, Run: llmLink,
			Doc: "Every recorded model call is tied to the reply it produced, and every request dump was readable."},
		{Name: "secrets.in_trace", Level: Fail, Run: secrets,
			Doc: "No key- or token-shaped string anywhere in the trace (messages, tool args, requests). Never prints the value."},
		{Name: "reply.unanswered_human_message", Level: Fail, Applies: telegramOnly, Run: unanswered,
			Doc: "A message the owner wrote got a text reply before the next one. Injected messages ([Cron delivery…]) are not the owner."},
		{Name: "tool.error", Level: Flag, Run: toolErrors,
			Doc: "A tool result reported an error (parsed from its JSON, not matched as text). Memory misses are counted separately."},
		{Name: "memory.miss", Level: Flag, Applies: telegramOnly, Run: memoryMisses,
			Doc: "memory.get named a document that does not exist: the agent guessed a path."},
		{Name: "loop.repeat_call", Level: Fail, Run: repeatedCalls,
			Doc: "The same tool with the same arguments three or more times."},
		{Name: "session.not_reset", Level: Flag, Applies: telegramOnly, Run: notReset,
			Doc: "You wrote to a session that had been silent for over an hour. A session reset policy (idle) would have started a fresh one."},
		{Name: "tools.surface", Level: Info, Run: toolSurface,
			Doc: "Tools used that are not gateway tools. Inventory only until there is an allow-list."},
		{Name: "ask.before_memory", Level: Flag, Applies: telegramOnly, Run: askBeforeMemory,
			Doc: "The agent asked a question and had made no memory call. Heuristic: the question detector is crude and the question may not have needed memory."},
	}
}

// ---- integrity ---------------------------------------------------------------

func unmatched(t *trace.Trace) []Finding {
	var out []Finding
	for _, g := range t.Unmatched {
		out = append(out, f(0, "gateway call #%d %s (%s) was not tied to any tool message", g.Seq, g.Tool, g.Decision))
	}
	return out
}

func orphanCalls(t *trace.Trace) []Finding {
	answered := map[string]bool{}
	for _, m := range t.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	var out []Finding
	for _, m := range t.Messages {
		for _, c := range m.Calls {
			if c.ID != "" && !answered[c.ID] {
				out = append(out, f(m.ID, "%s was called and no result came back", c.Name))
			}
		}
	}
	return out
}

func llmLink(t *trace.Trace) []Finding {
	if t.LLM == nil {
		return nil
	}
	var out []Finding
	for _, c := range t.LLM.Calls {
		if c.Reply == 0 {
			out = append(out, f(0, "model call %d is not tied to a reply (an API error, or the match failed)", c.N))
		}
	}
	for _, p := range t.LLM.Problems {
		out = append(out, f(0, "request dump unreadable: %s", p))
	}
	return out
}

// ---- secrets -----------------------------------------------------------------

var secretShapes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"Anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`)},
	{"API key (sk-)", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`)},
	{"Langfuse key", regexp.MustCompile(`\b[ps]k-lf-[A-Za-z0-9-]{8,}`)},
	{"AWS access key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"Telegram bot token", regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{35}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// secrets scans each message and each recorded request separately, so the finding can say
// where it is. It reports the kind and the place, never the matched text.
func secrets(t *trace.Trace) []Finding {
	var out []Finding
	scan := func(msg int64, where, text string) {
		for _, s := range secretShapes {
			if s.re.MatchString(text) {
				out = append(out, f(msg, "%s-shaped string in %s", s.kind, where))
			}
		}
	}
	for _, m := range t.Messages {
		scan(m.ID, "the "+m.Role+" message", m.Content)
		for _, c := range m.Calls {
			scan(m.ID, "the arguments of "+c.Name, string(c.Args))
		}
		if m.Gateway != nil {
			scan(m.ID, "the audit entry", string(m.Gateway.Args)+string(m.Gateway.Result))
		}
	}
	if l := t.LLM; l != nil {
		for k, v := range l.Systems {
			scan(0, "system prompt "+k, string(v))
		}
		for k, v := range l.ToolSets {
			scan(0, "tool set "+k, string(v))
		}
		for _, c := range l.Calls {
			for _, m := range c.New {
				scan(0, fmt.Sprintf("a message sent in model call %d", c.N), string(m))
			}
		}
	}
	return out
}

// ---- replies -----------------------------------------------------------------

// injected are user-role messages Hermes or the scheduler put in the conversation. They are
// not the owner, so they neither need a reply nor count as one.
var injectedPrefixes = []string{"[Cron delivery", "[ASYNC DELEGATION", "[System note", "[SYSTEM", "[IMPORTANT:"}

func isHuman(m trace.Message) bool {
	if m.Role != "user" {
		return false
	}
	c := strings.TrimSpace(m.Content)
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(c, p) {
			return false
		}
	}
	return true
}

// pendingGrace: a human message that is the last thing in a trace exported within this long
// of it may simply not have been answered yet.
const pendingGrace = 10 * time.Minute

func unanswered(t *trace.Trace) []Finding {
	var out []Finding
	ms := t.Messages
	for i, m := range ms {
		if !isHuman(m) || m.Inactive {
			continue
		}
		end := len(ms)
		for j := i + 1; j < len(ms); j++ {
			if isHuman(ms[j]) {
				end = j
				break
			}
		}
		answered := false
		for _, x := range ms[i+1 : end] {
			if x.Role == "assistant" && strings.TrimSpace(x.Content) != "" {
				answered = true
			}
		}
		if answered {
			continue
		}
		if end == len(ms) && !t.ExportedAt.IsZero() && t.ExportedAt.Sub(t.LastMessage) < pendingGrace {
			continue
		}
		out = append(out, f(m.ID, "no text reply before the next message of yours"))
	}
	return out
}

// notReset fires on a conversation that began, after a long silence, with a message the
// owner wrote: the session was still open when it should have been reset. A conversation
// that begins with an injected message (a cron delivery) is not this: the idle policy
// resets on the owner's next message, not on background traffic.
func notReset(t *trace.Trace) []Finding {
	if t.GapBeforeMinutes <= 0 || len(t.Messages) == 0 || !isHuman(t.Messages[0]) {
		return nil
	}
	return []Finding{f(t.Messages[0].ID, "continued session %s after %d minutes of silence; carried its earlier history", t.SessionID, t.GapBeforeMinutes)}
}

// ---- tools -------------------------------------------------------------------

// toolPayload returns the JSON object in a tool result, skipping the
// <untrusted_tool_result> preamble Hermes puts in front of gateway results.
func toolPayload(content string) map[string]any {
	i := strings.IndexByte(content, '{')
	if i < 0 {
		return nil
	}
	var v map[string]any
	if err := json.NewDecoder(strings.NewReader(content[i:])).Decode(&v); err != nil {
		return nil
	}
	return v
}

func errorText(p map[string]any) string {
	if e, ok := p["error"]; ok && e != nil {
		if s, isStr := e.(string); isStr {
			if strings.TrimSpace(s) != "" {
				return s
			}
		} else if txt := fmt.Sprint(e); txt != "" {
			return txt
		}
	}
	if s, ok := p["status"].(string); ok && s == "error" {
		return "status: error"
	}
	if ok, has := p["success"].(bool); has && !ok {
		return "success: false"
	}
	return ""
}

const memoryGet = "mcp__chhaya__memory_get"

func isMemoryMiss(m trace.Message) (string, bool) {
	if m.Role != "tool" || m.ToolName != memoryGet {
		return "", false
	}
	p := toolPayload(m.Content)
	if p == nil {
		return "", false
	}
	e, _ := p["error"].(string)
	if strings.HasPrefix(e, "no memory document") {
		return e, true
	}
	return "", false
}

func toolErrors(t *trace.Trace) []Finding {
	var out []Finding
	for _, m := range t.Messages {
		if m.Role != "tool" {
			continue
		}
		if _, miss := isMemoryMiss(m); miss {
			continue
		}
		if p := toolPayload(m.Content); p != nil {
			if e := errorText(p); e != "" {
				out = append(out, f(m.ID, "%s: %s", m.ToolName, clip(e, 140)))
			}
		}
	}
	return out
}

func memoryMisses(t *trace.Trace) []Finding {
	var out []Finding
	for _, m := range t.Messages {
		if e, miss := isMemoryMiss(m); miss {
			out = append(out, f(m.ID, "%s", e))
		}
	}
	return out
}

const repeatLimit = 3

func repeatedCalls(t *trace.Trace) []Finding {
	type key struct{ name, args string }
	count := map[key]int{}
	first := map[key]int64{}
	var order []key
	for _, m := range t.Messages {
		for _, c := range m.Calls {
			k := key{c.Name, canonical(c.Args)}
			if count[k] == 0 {
				first[k] = m.ID
				order = append(order, k)
			}
			count[k]++
		}
	}
	var out []Finding
	for _, k := range order {
		if count[k] >= repeatLimit {
			out = append(out, f(first[k], "%s called %d times with the same arguments", k.name, count[k]))
		}
	}
	return out
}

// canonical re-encodes JSON with sorted keys so argument order does not hide a repeat.
func canonical(raw json.RawMessage) string {
	var v any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func toolSurface(t *trace.Trace) []Finding {
	n := map[string]int{}
	for _, m := range t.Messages {
		for _, c := range m.Calls {
			if strings.HasPrefix(c.Name, "mcp__chhaya__") || c.Name == "tool_call" || c.Name == "tool_search" {
				continue
			}
			n[c.Name]++
		}
	}
	if len(n) == 0 {
		return nil
	}
	names := make([]string, 0, len(n))
	for k := range n {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s ×%d", k, n[k])
	}
	return []Finding{f(0, "non-gateway tools: %s", strings.Join(parts, ", "))}
}

// ---- behaviour ---------------------------------------------------------------

func askBeforeMemory(t *trace.Trace) []Finding {
	c := t.Counters
	if c.Questions > 0 && c.MemoryCallsBeforeFirstQuestion == 0 {
		return []Finding{f(c.FirstQuestionMessageID, "asked a question with no memory call before it (%d question(s) in the session)", c.Questions)}
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
