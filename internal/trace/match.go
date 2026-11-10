package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// matchWindow is how far apart a tool message and its audit entry may be. They are
// normally within milliseconds (checked on real data); five seconds is generous and
// still far below the gap between two calls.
const matchWindow = 5 * time.Second

// eventWindow is how long after a session's last message approver entries are still shown
// with it: a tap on an approval card can come long after the reply that asked for it.
// Overlapping sessions may both list the same event; that is harmless, an event is not a
// claim on a message.
const eventWindow = 30 * time.Minute

// ReadAudit reads audit.jsonl. A missing file is an error: an export with no audit log
// would show every gateway call as undecided, which is worse than failing.
func ReadAudit(path string) ([]GatewayEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	defer f.Close()
	return parseAudit(f)
}

func parseAudit(r io.Reader) ([]GatewayEntry, error) {
	var out []GatewayEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	line := 0
	for sc.Scan() {
		line++
		b := strings.TrimSpace(sc.Text())
		if b == "" {
			continue
		}
		var e GatewayEntry
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return nil, fmt.Errorf("audit log line %d: %w", line, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Matcher claims audit entries for tool messages. One matcher spans every session in an
// export, so two overlapping sessions cannot both claim the same gateway call.
type Matcher struct {
	entries []GatewayEntry
	used    map[int64]bool
}

func NewMatcher(entries []GatewayEntry) *Matcher {
	return &Matcher{entries: entries, used: map[int64]bool{}}
}

// Attach sets Gateway on each tool message whose tool is a gateway tool and whose
// audit entry (same tool, actor hermes, nearest in time within matchWindow) exists.
func (m *Matcher) Attach(msgs []Message) {
	for i := range msgs {
		if msgs[i].Role != "tool" {
			continue
		}
		want := GatewayName(msgs[i].ToolName)
		if want == "" {
			continue
		}
		best, bestD := -1, matchWindow+1
		for j, e := range m.entries {
			if m.used[e.Seq] || e.Actor != "hermes" || e.Tool != want {
				continue
			}
			d := e.TS.Sub(msgs[i].Time)
			if d < 0 {
				d = -d
			}
			if d <= matchWindow && d < bestD {
				best, bestD = j, d
			}
		}
		if best >= 0 {
			e := m.entries[best]
			m.used[e.Seq] = true
			msgs[i].Gateway = &e
		}
	}
}

// Residue returns, for a session spanning [from,to], the Hermes calls nothing claimed
// and the other actors' entries (approvals) up to eventWindow later, in time order.
func (m *Matcher) Residue(from, to time.Time) (unmatched, events []GatewayEntry) {
	from = from.Add(-matchWindow)
	for _, e := range m.entries {
		if e.TS.Before(from) {
			continue
		}
		switch {
		case e.Actor == "hermes" && !m.used[e.Seq] && !e.TS.After(to.Add(matchWindow)):
			unmatched = append(unmatched, e)
		case e.Actor != "hermes" && !e.TS.After(to.Add(eventWindow)):
			events = append(events, e)
		}
	}
	sort.Slice(unmatched, func(a, b int) bool { return unmatched[a].Seq < unmatched[b].Seq })
	sort.Slice(events, func(a, b int) bool { return events[a].Seq < events[b].Seq })
	return unmatched, events
}

// Count fills Counters from the messages.
func Count(msgs []Message) Counters {
	c := Counters{MemoryCallsBeforeFirstQuestion: -1}
	memory := 0
	for _, m := range msgs {
		switch m.Role {
		case "user":
			c.UserMessages++
		case "assistant":
			c.AssistantMessages++
			c.ToolCalls += len(m.Calls)
			for _, call := range m.Calls {
				if g := GatewayName(call.Name); g != "" {
					if c.GatewayCalls == nil {
						c.GatewayCalls = map[string]int{}
					}
					c.GatewayCalls[g]++
					if strings.HasPrefix(g, "memory.") {
						memory++
					}
				}
			}
			if len(m.Calls) == 0 && strings.Contains(m.Content, "?") {
				c.Questions++
				if c.MemoryCallsBeforeFirstQuestion < 0 {
					c.MemoryCallsBeforeFirstQuestion = memory
					c.FirstQuestionMessageID = m.ID
				}
			}
		}
	}
	return c
}
