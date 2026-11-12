// Package check runs deterministic checks over exported traces: plain Go over the trace
// JSON, no model and no Hermes run, so the same traces always give the same result.
// See docs/observability-and-evals.md ("Code checks before any judge").
//
// A check earns its place by agreeing with the owner's annotations. Until there are
// annotations, a check says what it saw and the owner decides whether it matters.
package check

import (
	"fmt"
	"sort"

	"github.com/satishthakur/chhaya/internal/trace"
)

// Level says what a finding means.
//
//	fail  an invariant is broken. The runner exits non-zero.
//	flag  look at this; it may or may not be wrong.
//	info  an inventory line, never a problem by itself.
type Level string

const (
	Fail Level = "fail"
	Flag Level = "flag"
	Info Level = "info"
)

// Finding is one thing a check saw in one trace. MessageID is 0 when it is about the
// trace as a whole. Detail never contains a secret value.
type Finding struct {
	Check     string `json:"check"`
	Level     Level  `json:"level"`
	MessageID int64  `json:"message_id,omitempty"`
	Detail    string `json:"detail"`
}

// Check is one named test. Applies narrows it to the traces it is meant for (a cron
// digest and a Telegram chat behave differently); nil means every trace.
type Check struct {
	Name    string
	Level   Level
	Doc     string
	Applies func(*trace.Trace) bool
	Run     func(*trace.Trace) []Finding
}

// TraceResult is what the checks said about one trace.
type TraceResult struct {
	ID       string    `json:"id"`
	Day      string    `json:"day"`
	Source   string    `json:"source"`
	Ran      []string  `json:"ran"`
	Findings []Finding `json:"findings"`
}

// Run applies every check that applies to t. A check cannot change the level of its own
// findings: the level belongs to the check.
func Run(t *trace.Trace, checks []Check) TraceResult {
	r := TraceResult{ID: t.ID, Day: t.Day, Source: t.Source, Findings: []Finding{}}
	for _, c := range checks {
		if c.Applies != nil && !c.Applies(t) {
			continue
		}
		r.Ran = append(r.Ran, c.Name)
		for _, f := range c.Run(t) {
			f.Check, f.Level = c.Name, c.Level
			r.Findings = append(r.Findings, f)
		}
	}
	return r
}

// Summary counts, per check, how many traces it ran on and how many it fired on.
type Summary struct {
	Check    string `json:"check"`
	Level    Level  `json:"level"`
	Ran      int    `json:"traces_ran"`
	Fired    int    `json:"traces_fired"`
	Findings int    `json:"findings"`
}

// Summarize totals results in the order of checks.
func Summarize(results []TraceResult, checks []Check) []Summary {
	idx := map[string]*Summary{}
	out := make([]Summary, len(checks))
	for i, c := range checks {
		out[i] = Summary{Check: c.Name, Level: c.Level}
		idx[c.Name] = &out[i]
	}
	for _, r := range results {
		for _, n := range r.Ran {
			if s := idx[n]; s != nil {
				s.Ran++
			}
		}
		fired := map[string]bool{}
		for _, f := range r.Findings {
			if s := idx[f.Check]; s != nil {
				s.Findings++
				fired[f.Check] = true
			}
		}
		for n := range fired {
			idx[n].Fired++
		}
	}
	return out
}

// Failures counts findings at level fail.
func Failures(results []TraceResult) int {
	n := 0
	for _, r := range results {
		for _, f := range r.Findings {
			if f.Level == Fail {
				n++
			}
		}
	}
	return n
}

// Sorted returns findings ordered by message id then check name, so output is stable.
func Sorted(fs []Finding) []Finding {
	out := append([]Finding(nil), fs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MessageID != out[j].MessageID {
			return out[i].MessageID < out[j].MessageID
		}
		return out[i].Check < out[j].Check
	})
	return out
}

func telegramOnly(t *trace.Trace) bool { return t.Source == "telegram" }

func f(msg int64, format string, a ...any) Finding {
	return Finding{MessageID: msg, Detail: fmt.Sprintf(format, a...)}
}
