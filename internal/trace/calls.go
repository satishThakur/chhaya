package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LLM is what Hermes actually sent to the model for one session, rebuilt from the
// request dumps it writes when HERMES_DUMP_REQUESTS=1 (sessions/request_dump_<id>_*.json).
// state.db keeps the conversation but not the system prompt, the tool schemas or what was
// injected into each request, so without the dumps a trace cannot show what the model saw.
//
// Each request re-sends the whole history, so the system prompt and the tool schemas are
// stored once (by hash) and each call stores only the messages added since the call before.
type LLM struct {
	Systems  map[string]json.RawMessage `json:"systems"`   // hash -> the system message as sent
	ToolSets map[string]json.RawMessage `json:"tool_sets"` // hash -> the tools array as sent
	Calls    []LLMCall                  `json:"calls"`
	// OtherDumps counts dumps that are not an ordinary request (error dumps). They are not
	// shown; the count is so a reader knows they exist.
	OtherDumps int      `json:"other_dumps,omitempty"`
	Problems   []string `json:"problems,omitempty"` // files that could not be read; never silent
}

// LLMCall is one request to the model.
type LLMCall struct {
	N               int    `json:"n"`
	Time            string `json:"time"` // as Hermes wrote it (its container's local time, no zone)
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	System          string `json:"system"` // key into LLM.Systems
	Tools           string `json:"tools"`  // key into LLM.ToolSets
	// Total is how many non-system messages the request held. New is the ones added since
	// the previous call; when the request is not an extension of the previous one
	// (history was compacted or rewritten) Reset is true and New holds all of them.
	Total    int  `json:"total"`
	Reset    bool `json:"reset,omitempty"`
	Attempts int  `json:"attempts,omitempty"` // >1 when the same request was retried
	// Carried is set on the first call of a later conversation in a split session: how many
	// of its messages came from before that conversation began. Hermes sent them anyway.
	Carried int               `json:"carried_messages,omitempty"`
	New     []json.RawMessage `json:"new"`
	// Reply is the state.db id of the assistant message this call produced, 0 if it could
	// not be established. Never guessed.
	Reply int64 `json:"reply_message_id,omitempty"`
}

type dumpFile struct {
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	Request   struct {
		Body map[string]json.RawMessage `json:"body"`
	} `json:"request"`
}

// ReadLLM rebuilds the model-side view of one session from the dumps in dir. It returns
// nil when the session has no ordinary dumps (the flag was off, or the dumps were pruned).
func ReadLLM(dir string, t *Trace) *LLM {
	if !safeID.MatchString(t.ID) {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "request_dump_"+t.ID+"_*.json"))
	if len(paths) == 0 {
		return nil
	}
	l := &LLM{Systems: map[string]json.RawMessage{}, ToolSets: map[string]json.RawMessage{}}
	var dumps []dumpFile
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var d dumpFile
		if err := json.Unmarshal(b, &d); err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if d.SessionID != t.ID {
			continue // a longer id that merely starts with this one
		}
		if d.Reason != "preflight" {
			l.OtherDumps++
			continue
		}
		dumps = append(dumps, d)
	}
	if len(dumps) == 0 {
		if l.OtherDumps == 0 && len(l.Problems) == 0 {
			return nil
		}
		return l
	}
	sort.SliceStable(dumps, func(i, j int) bool { return dumps[i].Timestamp < dumps[j].Timestamp })

	var prev []json.RawMessage // previous call's non-system messages, compacted
	var full [][]json.RawMessage
	for _, d := range dumps {
		var msgs []json.RawMessage
		if err := json.Unmarshal(d.Request.Body["messages"], &msgs); err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("%s: messages: %v", d.Timestamp, err))
			continue
		}
		var system json.RawMessage
		var rest []json.RawMessage
		for _, m := range msgs {
			m = compact(m)
			if system == nil && roleOf(m) == "system" {
				system = m
				continue
			}
			rest = append(rest, m)
		}
		sysKey := l.keep(l.Systems, system)
		toolKey := l.keep(l.ToolSets, compact(d.Request.Body["tools"]))

		c := LLMCall{
			N: len(l.Calls) + 1, Time: d.Timestamp, System: sysKey, Tools: toolKey, Total: len(rest),
			Model: unquote(d.Request.Body["model"]), ReasoningEffort: unquote(d.Request.Body["reasoning_effort"]),
		}
		switch {
		case prev == nil:
			c.New = rest
		case sameMessages(prev, rest):
			// A retry of the request before it: fold it in rather than list it twice.
			last := &l.Calls[len(l.Calls)-1]
			if last.Attempts == 0 {
				last.Attempts = 1
			}
			last.Attempts++
			continue
		case len(rest) > len(prev) && sameMessages(prev, rest[:len(prev)]):
			c.New = rest[len(prev):]
		default:
			c.Reset = true
			c.New = rest
		}
		l.Calls = append(l.Calls, c)
		full = append(full, rest)
		prev = rest
	}
	l.linkReplies(t, full)
	return l
}

// keep stores v under the hash of its contents and returns the hash.
func (l *LLM) keep(into map[string]json.RawMessage, v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	sum := sha256.Sum256(v)
	h := hex.EncodeToString(sum[:])[:12]
	into[h] = v
	return h
}

// linkReplies sets Reply on each call whose assistant message can be identified in the
// trace. A call's reply is the assistant message the next request carries at the position
// right after this one's messages; the last call has no next request, so it is linked only
// to the trace's final assistant message and only if nothing else claimed it.
func (l *LLM) linkReplies(t *Trace, full [][]json.RawMessage) {
	claimed := map[int64]bool{}
	find := func(fp string) int64 {
		for _, m := range t.Messages {
			if m.Role == "assistant" && !claimed[m.ID] && fingerprintState(m) == fp {
				return m.ID
			}
		}
		return 0
	}
	for i := range l.Calls {
		if i+1 >= len(l.Calls) {
			break
		}
		next := full[i+1]
		if l.Calls[i+1].Reset || len(next) <= l.Calls[i].Total || roleOf(next[l.Calls[i].Total]) != "assistant" {
			continue
		}
		if id := find(fingerprintDump(next[l.Calls[i].Total])); id != 0 {
			l.Calls[i].Reply = id
			claimed[id] = true
		}
	}
	if n := len(l.Calls); n > 0 {
		for j := len(t.Messages) - 1; j >= 0; j-- {
			if t.Messages[j].Role != "assistant" {
				continue
			}
			if !claimed[t.Messages[j].ID] {
				l.Calls[n-1].Reply = t.Messages[j].ID
			}
			break
		}
	}
}

func roleOf(m json.RawMessage) string {
	var x struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(m, &x)
	return x.Role
}

func compact(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return b
	}
	return buf.Bytes()
}

func unquote(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s
}

func sameMessages(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// fingerprintDump and fingerprintState reduce an assistant message to its text and its
// tool-call ids so the dump's copy can be matched to the state.db row.
func fingerprintDump(m json.RawMessage) string {
	var x struct {
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	_ = json.Unmarshal(m, &x)
	var ids []string
	for _, c := range x.ToolCalls {
		ids = append(ids, c.ID)
	}
	return strings.TrimSpace(contentText(x.Content)) + "\x00" + strings.Join(ids, ",")
}

func fingerprintState(m Message) string {
	var ids []string
	for _, c := range m.Calls {
		ids = append(ids, c.ID)
	}
	return strings.TrimSpace(m.Content) + "\x00" + strings.Join(ids, ",")
}

// contentText reads a message's content, which is a string or a list of text blocks.
func contentText(b json.RawMessage) string {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &blocks) == nil {
		var parts []string
		for _, bl := range blocks {
			parts = append(parts, bl.Text)
		}
		return strings.Join(parts, "")
	}
	return ""
}
