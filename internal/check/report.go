package check

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/satishthakur/chhaya/internal/trace"
)

// Load reads and parses every trace the store lists, for one IST day or (day == "") all.
// A trace that cannot be read or parsed is returned in problems, never skipped silently:
// a check that did not run is not a pass.
func Load(s *trace.Store, day string) (traces []*trace.Trace, problems []string, err error) {
	refs, err := s.Index()
	if err != nil {
		return nil, nil, err
	}
	for _, r := range refs {
		if day != "" && r.Day != day {
			continue
		}
		b, err := s.Trace(r.Day, r.ID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s/%s: %v", r.Day, r.ID, err))
			continue
		}
		var t trace.Trace
		if err := json.Unmarshal(b, &t); err != nil {
			problems = append(problems, fmt.Sprintf("%s/%s: not a trace: %v", r.Day, r.ID, err))
			continue
		}
		traces = append(traces, &t)
	}
	return traces, problems, nil
}

// WriteText prints the summary table and, for each trace with findings, the findings.
// info findings are shown only when verbose; they stay in the counts either way.
func WriteText(w io.Writer, results []TraceResult, checks []Check, problems []string, verbose bool) {
	withFindings := 0
	for _, r := range results {
		if hasShown(r, verbose) {
			withFindings++
		}
	}
	fmt.Fprintf(w, "checked %d trace(s): %d clean, %d with findings\n\n", len(results), len(results)-withFindings, withFindings)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "check\tlevel\tran on\tfired on\tfindings")
	for _, s := range Summarize(results, checks) {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", s.Check, s.Level, s.Ran, s.Fired, s.Findings)
	}
	tw.Flush()

	for _, r := range results {
		if !hasShown(r, verbose) {
			continue
		}
		fmt.Fprintf(w, "\n%s  %s  (%s)\n", r.Day, r.ID, r.Source)
		for _, x := range Sorted(r.Findings) {
			if x.Level == Info && !verbose {
				continue
			}
			where := ""
			if x.MessageID != 0 {
				where = fmt.Sprintf("msg %d  ", x.MessageID)
			}
			fmt.Fprintf(w, "  %-4s  %s  %s%s\n", x.Level, x.Check, where, oneLine(x.Detail))
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "\n%d trace(s) could NOT be checked:\n", len(problems))
		for _, p := range problems {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
	if n := Failures(results); n > 0 {
		fmt.Fprintf(w, "\n%d failure(s)\n", n)
	}
}

func hasShown(r TraceResult, verbose bool) bool {
	for _, x := range r.Findings {
		if x.Level != Info || verbose {
			return true
		}
	}
	return false
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// WriteJSON prints the whole report for another tool to read.
func WriteJSON(w io.Writer, results []TraceResult, checks []Check, problems []string) error {
	if problems == nil {
		problems = []string{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Summary  []Summary     `json:"summary"`
		Traces   []TraceResult `json:"traces"`
		Problems []string      `json:"problems"`
	}{Summarize(results, checks), results, problems})
}
