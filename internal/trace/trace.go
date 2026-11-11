// Package trace turns Hermes' session database and the gateway's audit log into one
// readable object per session (a "trace"), exports those to a directory and to S3, and
// serves them to a local annotation page. See docs/observability-and-evals.md.
//
// It only reads Hermes' state.db (through the sqlite3 CLI, read-only) and the audit log.
// It never writes to either, and it keeps no SQLite driver out of our Go code, for the
// same reason internal/backup does not have one.
package trace

import (
	"encoding/json"
	"strings"
	"time"
)

// Schema 2 adds LLM: the requests Hermes sent, when its request dumps were available.
const Schema = 2

// IST is the zone a trace's day folder is named in: my day, not UTC's.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Trace is one Hermes session: the conversation, each tool call, and the gateway's
// decision for each call that reached the gateway.
type Trace struct {
	Schema           int       `json:"schema"`
	ID               string    `json:"id"`
	Day              string    `json:"day"` // session start date in IST
	Source           string    `json:"source"`
	ChatID           string    `json:"chat_id,omitempty"`
	Model            string    `json:"model,omitempty"`
	Title            string    `json:"title,omitempty"`
	ParentID         string    `json:"parent_id,omitempty"`
	Started          time.Time `json:"started"`
	LastMessage      time.Time `json:"last_message"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	SystemPromptHash string    `json:"system_prompt_hash,omitempty"` // state.db keeps only the hash; the text comes from LLM
	MessageCount     int       `json:"message_count"`
	Messages         []Message `json:"messages"`
	// Unmatched are gateway calls by Hermes inside this session's time window that no
	// tool message claimed. Shown, never guessed away.
	Unmatched []GatewayEntry `json:"unmatched,omitempty"`
	// Events are gateway entries by other actors (the approver) inside the window.
	Events   []GatewayEntry `json:"events,omitempty"`
	Counters Counters       `json:"counters"`
	// LLM is what the model was sent on each call. Nil when Hermes was not dumping requests.
	LLM        *LLM      `json:"llm,omitempty"`
	ExportedAt time.Time `json:"exported_at"`
}

type Message struct {
	ID         int64         `json:"id"`
	Role       string        `json:"role"`
	Time       time.Time     `json:"time"`
	Content    string        `json:"content,omitempty"`
	Calls      []Call        `json:"calls,omitempty"`
	ToolName   string        `json:"tool_name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Inactive   bool          `json:"inactive,omitempty"` // compacted or replaced in Hermes' own history
	Gateway    *GatewayEntry `json:"gateway,omitempty"`
}

// Call is one tool call the assistant made. Hermes wraps most calls in a `tool_call`
// envelope; Via records that, and Name is the inner tool.
type Call struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Via  string          `json:"via,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// GatewayEntry is one audit-log record. Args are as the audit log stored them: already
// redacted by the gateway.
type GatewayEntry struct {
	Seq        int64           `json:"seq"`
	TS         time.Time       `json:"ts"`
	Actor      string          `json:"actor"`
	Tool       string          `json:"tool"`
	Kind       string          `json:"kind,omitempty"`
	Tier       int             `json:"tier"`
	Decision   string          `json:"decision"`
	Rule       string          `json:"rule,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Taint      json.RawMessage `json:"taint,omitempty"`
	Approval   json.RawMessage `json:"approval,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

// Counters are the numbers an eval check or a weekly report reads without re-parsing.
type Counters struct {
	UserMessages      int            `json:"user_messages"`
	AssistantMessages int            `json:"assistant_messages"`
	ToolCalls         int            `json:"tool_calls"`
	GatewayCalls      map[string]int `json:"gateway_calls,omitempty"`
	Questions         int            `json:"questions"`
	// MemoryCallsBeforeFirstQuestion is -1 when the assistant never asked a question.
	// A question is an assistant reply with no tool call that contains "?" (crude on
	// purpose; the page shows which message was counted).
	MemoryCallsBeforeFirstQuestion int   `json:"memory_calls_before_first_question"`
	FirstQuestionMessageID         int64 `json:"first_question_message_id,omitempty"`
}

// Day returns the IST calendar date of t, as 2006-01-02.
func Day(t time.Time) string { return t.In(IST).Format("2006-01-02") }

const gatewayPrefix = "mcp__chhaya__"

// GatewayName maps the tool name Hermes shows (mcp__chhaya__memory_propose_update) to the
// name in the audit log (memory.propose_update). "" means it is not a gateway tool.
func GatewayName(toolName string) string {
	n, ok := strings.CutPrefix(toolName, gatewayPrefix)
	if !ok || n == "" {
		return ""
	}
	return strings.Replace(n, "_", ".", 1)
}

// ParseCalls reads the tool_calls JSON of an assistant message. A malformed value yields
// one Call named "unparsed" holding the raw text, so nothing disappears silently.
func ParseCalls(raw string) []Call {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var items []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		q, _ := json.Marshal(raw)
		return []Call{{Name: "unparsed", Args: q}}
	}
	var out []Call
	for _, it := range items {
		args := rawJSON(it.Function.Arguments)
		if it.Function.Name == "tool_call" {
			var wrap struct {
				Calls []struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"calls"`
			}
			if json.Unmarshal(args, &wrap) == nil && len(wrap.Calls) > 0 {
				for _, c := range wrap.Calls {
					out = append(out, Call{ID: it.ID, Name: c.Name, Via: "tool_call", Args: c.Arguments})
				}
				continue
			}
		}
		out = append(out, Call{ID: it.ID, Name: it.Function.Name, Args: args})
	}
	return out
}

// rawJSON returns s as JSON if it is valid JSON, else as a JSON string.
func rawJSON(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	q, _ := json.Marshal(s)
	return q
}
