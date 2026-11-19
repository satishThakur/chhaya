// Package memtest is the structure of Personal memory's own tests: which facts exist, which
// probes and golden questions are written against them, and whether the two still agree.
//
// It runs no model. It checks the files, so a fact with no id, an id used twice, or a probe
// about a fact that was removed is found by code, the same way every night. Answering the
// probes with a weak model is the runner (docs/memory-selftest.md S2), a later step.
//
// The tests live in <memory root>/.tests/ (probes.json, golden.json): beside the memory so they
// are versioned and backed up with it, in a dot-directory the memory tools refuse to open
// (memory.Resolve), and never in the public repo, since a probe repeats the fact it tests.
package memtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Fact is one memory line with its provenance.
type Fact struct {
	ID   string // the proposal id: stable, unique, survives a move
	Path string // the document, as memory.get takes it: "taste/food"
	Text string
}

// Probe is a question about one fact, with the answer a weak model using only the memory tools
// must find. Kind is direct, indirect, negative (a fact that must not surface) or conflict
// (a superseded fact). A negative probe names no fact.
type Probe struct {
	ID       string `json:"id"`
	Fact     string `json:"fact,omitempty"`
	Kind     string `json:"kind"`
	Question string `json:"question"`
	Expected string `json:"expected"`
	Path     string `json:"path,omitempty"` // optional: where the answer should be found
}

// Golden is a real question with a known answer, taken from a session, that the improver never
// writes. ExpectAll are phrases (case-insensitive) a correct answer contains; ExpectNone are
// phrases it must not contain (a recommendation that repeats what the owner has already heard).
// At least one of the two is required, or the question can never fail. HeldOut ones are never
// shown to the improver at all. Source names the trace it came from.
type Golden struct {
	ID         string   `json:"id"`
	Question   string   `json:"question"`
	ExpectAll  []string `json:"expect_all,omitempty"`
	ExpectNone []string `json:"expect_none,omitempty"`
	Source     string   `json:"source,omitempty"`
	HeldOut    bool     `json:"held_out,omitempty"`
	// Pending marks a question known to fail until something outside the question is done (a fact
	// still to be proposed and approved). The runner reports it and does not count it as a failure.
	Pending bool   `json:"pending,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Problem is one thing wrong: Level "fail" is an invariant, "warn" is a gap to fill.
type Problem struct{ Level, Msg string }

// Report is what Check found.
type Report struct {
	Facts    []Fact
	Probes   []Probe
	Golden   []Golden
	Problems []Problem
}

// Failures counts the problems at level fail.
func (r Report) Failures() int {
	n := 0
	for _, p := range r.Problems {
		if p.Level == "fail" {
			n++
		}
	}
	return n
}

var (
	provenance = regexp.MustCompile(`<!-- proposal (\d{8}-[0-9a-f]{16}), approved \d{4}-\d{2}-\d{2} -->\s*$`)
	kinds      = map[string]bool{"direct": true, "indirect": true, "negative": true, "conflict": true}
)

// Check reads the memory at root and its .tests directory. It returns an error only when a file
// cannot be read or has an unknown field: a test file that did not load is not a pass.
func Check(root string) (Report, error) {
	var r Report
	if err := readFacts(root, &r); err != nil {
		return r, err
	}
	if err := load(filepath.Join(root, ".tests", "probes.json"), &r.Probes); err != nil {
		return r, err
	}
	if err := load(filepath.Join(root, ".tests", "golden.json"), &r.Golden); err != nil {
		return r, err
	}
	r.checkProbes()
	r.checkGolden()
	return r, nil
}

func (r *Report) add(level, format string, a ...any) {
	r.Problems = append(r.Problems, Problem{level, fmt.Sprintf(format, a...)})
}

func readFacts(root string, r *Report) error {
	var docs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			docs = append(docs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(docs)
	seen := map[string]string{}
	for _, p := range docs {
		rel, _ := filepath.Rel(root, p)
		path := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, ln := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(ln, "- ") {
				continue
			}
			m := provenance.FindStringSubmatch(ln)
			if m == nil {
				r.add("fail", "%s line %d has no proposal id: %s", path, i+1, clip(ln, 60))
				continue
			}
			if prev, dup := seen[m[1]]; dup {
				r.add("fail", "id %s is on two lines (%s and %s)", m[1], prev, path)
				continue
			}
			seen[m[1]] = path
			text := strings.TrimSpace(strings.TrimPrefix(provenance.ReplaceAllString(ln, ""), "- "))
			r.Facts = append(r.Facts, Fact{ID: m[1], Path: path, Text: text})
		}
	}
	return nil
}

func (r *Report) checkProbes() {
	facts := map[string]bool{}
	for _, f := range r.Facts {
		facts[f.ID] = true
	}
	covered := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range r.Probes {
		switch {
		case p.ID == "":
			r.add("fail", "a probe has no id (question %q)", clip(p.Question, 40))
		case ids[p.ID]:
			r.add("fail", "probe id %s is used twice", p.ID)
		}
		ids[p.ID] = true
		if !kinds[p.Kind] {
			r.add("fail", "probe %s: kind %q is not direct, indirect, negative or conflict", p.ID, p.Kind)
		}
		if strings.TrimSpace(p.Question) == "" || strings.TrimSpace(p.Expected) == "" {
			r.add("fail", "probe %s: question and expected are both required", p.ID)
		}
		switch {
		case p.Kind == "negative" && p.Fact == "":
		case p.Fact == "":
			r.add("fail", "probe %s names no fact", p.ID)
		case !facts[p.Fact]:
			r.add("fail", "probe %s is about fact %s, which is not in memory (removed, or a typo)", p.ID, p.Fact)
		default:
			covered[p.Fact] = true
		}
	}
	for _, f := range r.Facts {
		if !covered[f.ID] {
			r.add("warn", "fact %s (%s) has no probe: %s", f.ID, f.Path, clip(f.Text, 50))
		}
	}
}

func (r *Report) checkGolden() {
	ids := map[string]bool{}
	for _, g := range r.Golden {
		if g.ID == "" || ids[g.ID] {
			r.add("fail", "golden question has a missing or repeated id %q", g.ID)
		}
		ids[g.ID] = true
		if strings.TrimSpace(g.Question) == "" {
			r.add("fail", "golden %s has no question", g.ID)
		}
		if len(g.ExpectAll)+len(g.ExpectNone) == 0 {
			r.add("fail", "golden %s expects nothing, so it can never fail", g.ID)
		}
	}
}

// load reads a JSON array, refusing unknown fields. A missing file is an empty set.
func load(path string, into any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// FactsIn returns the fact lines of one document's text, as memory.get returns it. A line with no
// proposal id is skipped here; Check is what reports it.
func FactsIn(path, body string) []Fact {
	var out []Fact
	for _, ln := range strings.Split(body, "\n") {
		if !strings.HasPrefix(ln, "- ") {
			continue
		}
		m := provenance.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(provenance.ReplaceAllString(ln, ""), "- "))
		out = append(out, Fact{ID: m[1], Path: path, Text: text})
	}
	return out
}
