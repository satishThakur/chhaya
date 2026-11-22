package dream

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/trace"
)

// RunOptions. Budget is the most proposals for the whole run (the gateway's daily cap is the
// backstop). Propose false is a dry run: the agent runs and the report says what it would have
// sent, but nothing reaches the gateway and nothing is marked done. Ledger may be nil (no memory
// of what was read).
type RunOptions struct {
	PerTrace int // most proposals from one conversation; 0 means 5
	Budget   int
	Propose  bool
	Ledger   *Ledger
}

// TraceResult is what the agent did with one conversation, kept so a report can show it. Quotes
// maps each cited owner message id to its text, so a reviewer sees what the line came from.
type TraceResult struct {
	ID, Day        string
	Messages       int
	Proposed       []Candidate
	Contradictions []Candidate
	Refused        []Drop
	Quotes         map[int64]string
	First          string // the owner's first message, so a reader can tell conversations apart
	Err            string
}

// Summary counts what a run did.
type Summary struct {
	Results                                                            []TraceResult
	Read, Skipped, Deferred, Errors, Proposed, Contradictions, Refused int
	Usage                                                              memrun.Usage
}

type dryGateway struct{ Gateway }

func (dryGateway) Propose(context.Context, string, string, string) error { return nil }

func ts(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }

func hasOwnerMessage(t *trace.Trace) bool {
	for _, m := range t.Messages {
		if check.IsHuman(m) {
			return true
		}
	}
	return false
}

// RunTraces reads each eligible conversation, oldest first, with the agent. A conversation that
// is not an owner conversation, has no owner message or was already read is skipped; one that would
// start after the budget is used up is deferred, and left unmarked for the next run; one whose agent
// fails is left unmarked and counted. The error is non-nil if any agent failed.
func RunTraces(ctx context.Context, model memrun.Model, gw Gateway, traces []*trace.Trace, o RunOptions, out io.Writer) (Summary, error) {
	var s Summary
	sort.SliceStable(traces, func(i, j int) bool { return traces[i].LastMessage.Before(traces[j].LastMessage) })
	budget := o.Budget
	if !o.Propose {
		gw = dryGateway{gw}
	}
	var prior []Candidate // proposed so far in this run: waiting for a tap, not in memory yet
	for _, t := range traces {
		last := t.LastMessage.UTC().Format(time.RFC3339)
		switch {
		case !Eligible(t) || !hasOwnerMessage(t):
			s.Skipped++
			continue
		case o.Ledger != nil && o.Propose && o.Ledger.Seen(t.ID, last):
			s.Skipped++
			continue
		case budget <= 0:
			s.Deferred++
			fmt.Fprintf(out, "%s  deferred: today's proposal budget is used; it is read on the next run\n", t.ID)
			continue
		}
		fmt.Fprintf(out, "%s  (%s, %d messages)\n", t.ID, t.Day, len(t.Messages))
		per := o.PerTrace
		if per <= 0 {
			per = 5
		}
		if budget < per {
			per = budget
		}
		r, err := RunAgent(ctx, model, gw, t, per, prior...)
		s.Usage.In, s.Usage.Out = s.Usage.In+r.Usage.In, s.Usage.Out+r.Usage.Out
		tr := TraceResult{ID: t.ID, Day: t.Day, Messages: len(t.Messages), Proposed: r.Proposed,
			Contradictions: r.Contradictions, Refused: r.Refused, Quotes: quotes(t, r), First: firstOwner(t)}
		if err != nil {
			s.Errors++
			tr.Err = err.Error()
			s.Results = append(s.Results, tr)
			fmt.Fprintf(out, "  ERROR: %v (left for the next run)\n", err)
			continue
		}
		s.Results = append(s.Results, tr)
		prior = append(prior, r.Proposed...)
		s.Read++
		verb := "proposed"
		if !o.Propose {
			verb = "would propose (dry run)"
		}
		for _, p := range r.Proposed {
			fmt.Fprintf(out, "  %s: [%s] %s\n", verb, p.Path, p.Entry)
		}
		for _, c := range r.Contradictions {
			fmt.Fprintf(out, "  contradiction (nothing added): [%s] %s  reverses %s\n", c.Path, c.Entry, c.Contradicts)
		}
		for _, d := range r.Refused {
			fmt.Fprintf(out, "  refused: %q (%s)\n", clip(d.Candidate.Entry, 70), d.Reason)
		}
		if len(r.Proposed)+len(r.Contradictions) == 0 {
			fmt.Fprintln(out, "  nothing to save")
		}
		s.Proposed += len(r.Proposed)
		s.Contradictions += len(r.Contradictions)
		s.Refused += len(r.Refused)
		budget -= len(r.Proposed)
		if o.Ledger != nil && o.Propose {
			if err := o.Ledger.Mark(t.ID, last); err != nil {
				return s, fmt.Errorf("could not record that %s was read: %w", t.ID, err)
			}
		}
	}
	if s.Errors > 0 {
		return s, fmt.Errorf("%d conversation(s) failed", s.Errors)
	}
	return s, nil
}

// quotes returns the text of every owner message the agent cited.
func quotes(t *trace.Trace, r AgentResult) map[int64]string {
	want := map[int64]bool{}
	for _, c := range r.Proposed {
		for _, id := range c.Source {
			want[id] = true
		}
	}
	for _, c := range r.Contradictions {
		for _, id := range c.Source {
			want[id] = true
		}
	}
	for _, d := range r.Refused {
		for _, id := range d.Candidate.Source {
			want[id] = true
		}
	}
	out := map[int64]string{}
	for _, m := range t.Messages {
		if want[m.ID] && check.IsHuman(m) {
			out[m.ID] = clip(m.Content, 600)
		}
	}
	return out
}

func firstOwner(t *trace.Trace) string {
	for _, m := range t.Messages {
		if check.IsHuman(m) {
			return clip(m.Content, 140)
		}
	}
	return ""
}
