package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Query runs one SQL statement against a database file and returns the rows as a JSON
// array (the sqlite3 CLI's -json output). Injected so tests can use a fixture database
// or a fake.
type Query func(dbPath, sql string) ([]byte, error)

// SQLiteQuery is the real Query: the sqlite3 CLI, opened read-only. It fails closed when
// the CLI is missing rather than returning no rows.
func SQLiteQuery(dbPath, sql string) ([]byte, error) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, errors.New("sqlite3 is not installed (apt install sqlite3)")
	}
	out, err := exec.Command(bin, "-readonly", "-json", dbPath, sql).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("sqlite3: %w", err)
	}
	return out, nil
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type sessionRow struct {
	ID               string   `json:"id"`
	Source           string   `json:"source"`
	ChatID           *string  `json:"chat_id"`
	Model            *string  `json:"model"`
	Title            *string  `json:"title"`
	ParentID         *string  `json:"parent_session_id"`
	StartedAt        float64  `json:"started_at"`
	LastTS           *float64 `json:"last_ts"`
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	EstimatedCost    *float64 `json:"estimated_cost_usd"`
	ActualCost       *float64 `json:"actual_cost_usd"`
	SystemPromptHash *string  `json:"system_prompt_hash"`
}

type messageRow struct {
	ID         int64   `json:"id"`
	SessionID  string  `json:"session_id"`
	Role       string  `json:"role"`
	Content    *string `json:"content"`
	ToolCallID *string `json:"tool_call_id"`
	ToolCalls  *string `json:"tool_calls"`
	ToolName   *string `json:"tool_name"`
	Finish     *string `json:"finish_reason"`
	Timestamp  float64 `json:"timestamp"`
	Active     *int    `json:"active"`
	Compacted  *int    `json:"compacted"`
}

func decode[T any](b []byte, what string) ([]T, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil // sqlite3 -json prints nothing for zero rows
	}
	var rows []T
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return rows, nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func epoch(f float64) time.Time {
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
}

// ReadSessions returns a trace (without gateway matching) for every session that has
// messages and whose last message is at or after since. A zero since means all of them.
// The query names every column it uses, so a Hermes upgrade that renames one fails here,
// loudly, instead of producing empty traces.
func ReadSessions(dbPath string, since time.Time, q Query) ([]*Trace, error) {
	where := " WHERE last_ts IS NOT NULL"
	if !since.IsZero() {
		where += fmt.Sprintf(" AND last_ts >= %f", float64(since.UnixNano())/1e9)
	}
	sb, err := q(dbPath, `SELECT * FROM (SELECT s.id, s.source, s.chat_id, s.model, s.title,
		s.parent_session_id, s.started_at, s.input_tokens, s.output_tokens,
		s.estimated_cost_usd, s.actual_cost_usd, s.system_prompt_hash,
		(SELECT max(timestamp) FROM messages m WHERE m.session_id = s.id) AS last_ts
		FROM sessions s)`+where+` ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	srows, err := decode[sessionRow](sb, "sessions")
	if err != nil {
		return nil, err
	}
	var out []*Trace
	for _, r := range srows {
		if !safeID.MatchString(r.ID) {
			return nil, fmt.Errorf("session id %q has unexpected characters; refusing to build a path from it", r.ID)
		}
		t := &Trace{
			Schema: Schema, ID: r.ID, Source: r.Source, ChatID: str(r.ChatID), Model: str(r.Model),
			Title: str(r.Title), ParentID: str(r.ParentID), Started: epoch(r.StartedAt),
			SystemPromptHash: str(r.SystemPromptHash),
		}
		t.Day = Day(t.Started)
		if r.LastTS != nil {
			t.LastMessage = epoch(*r.LastTS)
		}
		if r.InputTokens != nil {
			t.InputTokens = *r.InputTokens
		}
		if r.OutputTokens != nil {
			t.OutputTokens = *r.OutputTokens
		}
		if r.ActualCost != nil {
			t.CostUSD = *r.ActualCost
		} else if r.EstimatedCost != nil {
			t.CostUSD = *r.EstimatedCost
		}
		mb, err := q(dbPath, fmt.Sprintf(`SELECT id, session_id, role, content, tool_call_id, tool_calls,
			tool_name, finish_reason, timestamp, active, compacted FROM messages WHERE session_id = '%s' ORDER BY id`, r.ID))
		if err != nil {
			return nil, err
		}
		mrows, err := decode[messageRow](mb, "messages of "+r.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range mrows {
			msg := Message{
				ID: m.ID, Role: m.Role, Time: epoch(m.Timestamp), Content: str(m.Content),
				ToolName: str(m.ToolName), ToolCallID: str(m.ToolCallID), FinishReason: str(m.Finish),
				Inactive: (m.Active != nil && *m.Active == 0) || (m.Compacted != nil && *m.Compacted != 0),
			}
			if m.ToolCalls != nil {
				msg.Calls = ParseCalls(*m.ToolCalls)
			}
			t.Messages = append(t.Messages, msg)
		}
		out = append(out, t)
	}
	return out, nil
}
