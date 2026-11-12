package trace

import (
	"encoding/json"
	"fmt"
	"time"
)

// DefaultGap is how long a silence inside one Hermes session starts a new conversation.
// Hermes keeps a Telegram chat as one session until /new, so a session can hold days of
// unrelated conversations; one trace is one conversation (several turns), not a whole session.
const DefaultGap = 60 * time.Minute

// Split divides a session's trace into conversations wherever the time between two
// consecutive messages is longer than gap. The first conversation keeps the session id and
// day, so a session that never pauses exports exactly as before; later ones are <id>.2,
// <id>.3 and so on, filed under the day they began. gap <= 0 turns splitting off.
//
// The caller has already joined the audit log and read the request dumps for the whole
// session; Split only partitions what is there. Session totals (tokens, cost) cannot be
// divided, so they stay on the first conversation and are zero on the rest.
func Split(t *Trace, gap time.Duration) []*Trace {
	cuts := []int{0}
	if gap > 0 {
		for i := 1; i < len(t.Messages); i++ {
			a, b := t.Messages[i-1].Time, t.Messages[i].Time
			if !a.IsZero() && !b.IsZero() && b.Sub(a) > gap {
				cuts = append(cuts, i)
			}
		}
	}
	if len(cuts) == 1 {
		return []*Trace{t}
	}
	cuts = append(cuts, len(t.Messages))
	n := len(cuts) - 1
	segs := make([]*Trace, n)
	segOf := map[int64]int{}
	for k := 0; k < n; k++ {
		ms := t.Messages[cuts[k]:cuts[k+1]]
		s := &Trace{
			Schema: t.Schema, ID: t.ID, Day: t.Day, Source: t.Source, ChatID: t.ChatID, Model: t.Model,
			Title: t.Title, ParentID: t.ParentID, SystemPromptHash: t.SystemPromptHash, ExportedAt: t.ExportedAt,
			Started: t.Started, Messages: ms, MessageCount: len(ms), Counters: Count(ms),
			SessionID: t.ID, Segment: k + 1, Segments: n,
		}
		s.LastMessage = ms[len(ms)-1].Time
		if k == 0 {
			s.InputTokens, s.OutputTokens, s.CostUSD = t.InputTokens, t.OutputTokens, t.CostUSD
		} else {
			s.ID = fmt.Sprintf("%s.%d", t.ID, k+1)
			s.Started = ms[0].Time
			s.Day = Day(s.Started)
			s.GapBeforeMinutes = int(ms[0].Time.Sub(t.Messages[cuts[k]-1].Time) / time.Minute)
		}
		for _, m := range ms {
			segOf[m.ID] = k
		}
		segs[k] = s
	}
	assign := func(list []GatewayEntry, into func(*Trace, GatewayEntry)) {
		for _, e := range list {
			k := 0
			for j := 0; j < n; j++ {
				if !e.TS.Before(segs[j].Started.Add(-matchWindow)) {
					k = j
				}
			}
			into(segs[k], e)
		}
	}
	assign(t.Unmatched, func(s *Trace, e GatewayEntry) { s.Unmatched = append(s.Unmatched, e) })
	assign(t.Events, func(s *Trace, e GatewayEntry) { s.Events = append(s.Events, e) })
	splitLLM(t.LLM, segs, segOf)
	return segs
}

// splitLLM hands each conversation the model calls that produced its replies. A call with
// no linked reply follows the call before it. The first call of a later conversation is
// stored in full, because Hermes sent it the whole session so far: that carried-over
// history is exactly what a reader needs to see.
func splitLLM(l *LLM, segs []*Trace, segOf map[int64]int) {
	if l == nil {
		return
	}
	full := make([][]json.RawMessage, len(l.Calls))
	var cur []json.RawMessage
	for i, c := range l.Calls {
		if c.Reset {
			cur = append([]json.RawMessage(nil), c.New...)
		} else {
			cur = append(cur[:len(cur):len(cur)], c.New...)
		}
		full[i] = cur
	}
	seg := make([]int, len(l.Calls))
	last := -1
	for i, c := range l.Calls {
		if k, ok := segOf[c.Reply]; ok && c.Reply != 0 {
			seg[i] = k
			last = k
		} else if last >= 0 {
			seg[i] = last
		} else {
			seg[i] = -1 // before any linked call; resolved below
		}
	}
	for i := len(seg) - 1; i >= 0; i-- {
		if seg[i] == -1 {
			seg[i] = 0
			for j := i + 1; j < len(seg); j++ {
				if seg[j] != -1 {
					seg[i] = seg[j]
					break
				}
			}
		}
	}
	parts := make([]*LLM, len(segs))
	for i, c := range l.Calls {
		k := seg[i]
		p := parts[k]
		if p == nil {
			p = &LLM{Systems: map[string]json.RawMessage{}, ToolSets: map[string]json.RawMessage{}}
			parts[k] = p
		}
		if v, ok := l.Systems[c.System]; ok {
			p.Systems[c.System] = v
		}
		if v, ok := l.ToolSets[c.Tools]; ok {
			p.ToolSets[c.Tools] = v
		}
		if k > 0 && len(p.Calls) == 0 {
			c.Reset, c.New, c.Carried = true, full[i], carriedOver(c, segs[k])
		}
		p.Calls = append(p.Calls, c)
	}
	for k, s := range segs {
		if k == 0 && parts[0] != nil {
			parts[0].OtherDumps, parts[0].Problems = l.OtherDumps, l.Problems
		}
		if parts[k] == nil && k == 0 && (l.OtherDumps > 0 || len(l.Problems) > 0) {
			parts[0] = &LLM{Systems: map[string]json.RawMessage{}, ToolSets: map[string]json.RawMessage{}, OtherDumps: l.OtherDumps, Problems: l.Problems}
		}
		s.LLM = parts[k]
	}
}

// carriedOver estimates how many of a request's messages came from before the conversation
// began: the request's size less the conversation's own messages that precede its reply
// (the ones Hermes sends to the model: user, assistant and tool). It assumes Hermes sent
// every stored message, which holds unless it compacted the history; the page says "carries".
func carriedOver(c LLMCall, s *Trace) int {
	own := 0
	for _, m := range s.Messages {
		if c.Reply != 0 && m.ID >= c.Reply {
			break
		}
		if m.Role == "user" || m.Role == "assistant" || m.Role == "tool" {
			own++
		}
	}
	if n := c.Total - own; n > 0 {
		return n
	}
	return 0
}
