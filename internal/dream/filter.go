// Package dream is the extractor half of the dreaming pipeline (docs/dreaming.md): it reads one
// conversation and the current memory and proposes the facts the owner stated and memory lacks.
//
// What is policy lives here, in code, and not in the prompt: the source of a fact must be a message
// the owner typed, a line must be one valid line for a document that exists, a repeat of a stored
// fact is dropped, a contradiction is reported and never added, and the number of proposals is
// capped. The model only suggests; Filter decides.
package dream

import (
	"strings"
	"unicode"

	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/memtest"
	"github.com/satishthakur/chhaya/internal/trace"
)

// Candidate is one suggested change. Source is the ids of the owner messages it comes from.
// Contradicts, when set, is the id of the stored fact it reverses.
type Candidate struct {
	Path        string  `json:"path"`
	Entry       string  `json:"entry"`
	Rationale   string  `json:"rationale"`
	Source      []int64 `json:"source"`
	Contradicts string  `json:"contradicts,omitempty"`
}

// Input is what the extractor sees: one conversation, the documents that exist, the stored facts, and
// the most it may propose.
type Input struct {
	Trace *trace.Trace
	Docs  []string
	Facts []memtest.Fact
	Cap   int
}

// Drop is a candidate that was refused, and why.
type Drop struct {
	Candidate Candidate
	Reason    string
}

// Result is what survived. Propose is what goes to the owner as an add; Contradictions are shown
// with both lines and never added; OverCap counts the valid candidates the cap cut.
type Result struct {
	Propose        []Candidate
	Contradictions []Candidate
	Dropped        []Drop
	OverCap        int
}

// Eligible reports whether a trace may be read for facts: an owner conversation on Telegram. A cron
// run (the digest) holds no owner-typed facts.
func Eligible(t *trace.Trace) bool { return t != nil && t.Source == "telegram" }

// norm makes two spellings of one line compare equal.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Filter applies the rules to what the model suggested, in the model's order.
func Filter(cands []Candidate, in Input) Result {
	var r Result
	owner := map[int64]bool{}
	if in.Trace != nil {
		for _, m := range in.Trace.Messages {
			if check.IsHuman(m) {
				owner[m.ID] = true
			}
		}
	}
	docs := map[string]bool{}
	for _, d := range in.Docs {
		docs[d] = true
	}
	ids := map[string]bool{}
	seen := map[string]bool{}
	for _, f := range in.Facts {
		ids[f.ID] = true
		seen[norm(f.Text)] = true
	}
	drop := func(c Candidate, why string) { r.Dropped = append(r.Dropped, Drop{c, why}) }
	for _, c := range cands {
		entry, err := memory.ValidateEntry(c.Entry)
		if err != nil {
			drop(c, "bad line: "+err.Error())
			continue
		}
		c.Entry = entry
		if !docs[c.Path] {
			drop(c, "no such document: "+c.Path)
			continue
		}
		if len(c.Source) == 0 {
			drop(c, "no source message cited")
			continue
		}
		ok := true
		for _, id := range c.Source {
			if !owner[id] {
				ok = false
			}
		}
		if !ok {
			drop(c, "source is not a message the owner typed")
			continue
		}
		if c.Contradicts != "" {
			if !ids[c.Contradicts] {
				drop(c, "contradicts a fact that does not exist: "+c.Contradicts)
				continue
			}
			r.Contradictions = append(r.Contradictions, c)
			continue
		}
		if seen[norm(entry)] {
			drop(c, "duplicate of a stored or earlier line")
			continue
		}
		seen[norm(entry)] = true
		if len(r.Propose) >= in.Cap {
			r.OverCap++
			continue
		}
		r.Propose = append(r.Propose, c)
	}
	return r
}
