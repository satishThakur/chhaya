package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/memtest"
	"github.com/satishthakur/chhaya/internal/trace"
)

// Want is one proposal a case expects: the document, phrases the line must contain
// (case-insensitive), and optionally the owner message ids it must cite.
type Want struct {
	Path   string   `json:"path"`
	All    []string `json:"all"`
	Source []int64  `json:"source,omitempty"`
}

// Expect is the whole outcome a case requires. Count defaults to len(Propose): a silence case
// expects none, and an extra line is a failure, because noise costs the owner a tap and their
// trust. Contradict lists stored fact ids that must come back as contradictions. OverCap is how
// many valid lines the cap must cut.
type Expect struct {
	Propose    []Want   `json:"propose,omitempty"`
	Count      *int     `json:"count,omitempty"`
	Contradict []string `json:"contradict,omitempty"`
	OverCap    int      `json:"over_cap,omitempty"`
}

// Case is one eval: a memory, a conversation, the cap and what must happen. Memory maps a document
// to its lines (each with a proposal id comment, as real memory has).
type Case struct {
	ID     string              `json:"id"`
	Note   string              `json:"note,omitempty"`
	Cap    int                 `json:"cap"`
	Memory map[string][]string `json:"memory"`
	// Prior are proposals an earlier conversation of the same run already made (path and entry).
	Prior []struct {
		Path  string `json:"path"`
		Entry string `json:"entry"`
	} `json:"prior,omitempty"`
	Trace  *trace.Trace `json:"trace"`
	Expect Expect       `json:"expect"`
}

// ParseCase reads one case; an unknown field or a missing part is an error, so a typo cannot make
// a case that always passes.
func ParseCase(b []byte) (Case, error) {
	var c Case
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	switch {
	case c.ID == "":
		return c, fmt.Errorf("case has no id")
	case c.Trace == nil:
		return c, fmt.Errorf("case %s has no trace", c.ID)
	case c.Memory == nil:
		return c, fmt.Errorf("case %s has no memory", c.ID)
	}
	return c, nil
}

// LoadCases reads every *.json in dir, sorted by name.
func LoadCases(dir string) ([]Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Case
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		c, err := ParseCase(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// CaseResult is what one case did.
type CaseResult struct {
	ID      string
	Pass    bool
	Why     []string // what was wrong, empty on a pass
	Result  Result
	Usage   memrun.Usage
	ModelOK bool
}

// inputFor builds the extractor's input from a case, through the same fact parser real memory uses.
func inputFor(c Case) (Input, error) {
	dir, err := os.MkdirTemp("", "chhaya-dream-case-")
	if err != nil {
		return Input{}, err
	}
	defer os.RemoveAll(dir)
	var docs []string
	for path, lines := range c.Memory {
		docs = append(docs, path)
		p := filepath.Join(dir, filepath.FromSlash(path)+".md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Input{}, err
		}
		body := "# " + path + "\n\n"
		for _, l := range lines {
			body += "- " + l + "\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return Input{}, err
		}
	}
	sort.Strings(docs)
	rep, err := memtest.Check(dir)
	if err != nil {
		return Input{}, err
	}
	for _, p := range rep.Problems {
		if p.Level == "fail" {
			return Input{}, fmt.Errorf("case memory: %s", p.Msg)
		}
	}
	return Input{Trace: c.Trace, Docs: docs, Facts: rep.Facts, Cap: c.Cap}, nil
}

// memGateway is a Gateway over a case's memory. It records what was proposed and writes nothing.
type memGateway struct{ docs map[string]string }

func newMemGateway(c Case) *memGateway {
	g := &memGateway{docs: map[string]string{}}
	for path, lines := range c.Memory {
		body := "# " + path + "\n\n"
		for _, l := range lines {
			body += "- " + l + "\n"
		}
		g.docs[path] = body
	}
	return g
}

func (g *memGateway) List(context.Context) ([]string, error) {
	var out []string
	for k := range g.docs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (g *memGateway) Get(_ context.Context, p string) (string, error) {
	d, ok := g.docs[p]
	if !ok {
		return "", fmt.Errorf("no such document: %s", p)
	}
	return d, nil
}

func (g *memGateway) Propose(context.Context, string, string, string) error { return nil }

// RunCase runs the agent on a case, against a fake gateway over the case's memory, and checks the
// outcome. A model error, or an agent that does not finish, is a failed case with the reason.
func RunCase(ctx context.Context, model memrun.Model, c Case) CaseResult {
	r := CaseResult{ID: c.ID}
	if _, err := inputFor(c); err != nil { // the case's memory must parse like real memory
		r.Why = []string{err.Error()}
		return r
	}
	var prior []Candidate
	for _, p := range c.Prior {
		prior = append(prior, Candidate{Path: p.Path, Entry: p.Entry})
	}
	ar, err := RunAgent(ctx, model, newMemGateway(c), c.Trace, c.Cap, prior...)
	r.Usage = ar.Usage
	if err != nil {
		r.Why = []string{"agent: " + err.Error()}
		return r
	}
	r.ModelOK = true
	r.Result = Result{Propose: ar.Proposed, Contradictions: ar.Contradictions, Dropped: ar.Refused, OverCap: ar.OverCap}
	why := func(f string, a ...any) { r.Why = append(r.Why, fmt.Sprintf(f, a...)) }

	want := len(c.Expect.Propose)
	if c.Expect.Count != nil {
		want = *c.Expect.Count
	}
	if got := len(r.Result.Propose); got != want {
		why("expected %d proposal(s), got %d: %s", want, got, entries(r.Result.Propose))
	}
	for _, w := range c.Expect.Propose {
		if !matches(r.Result.Propose, w) {
			why("no proposal for %s containing %v%s", w.Path, w.All, srcNote(w.Source))
		}
	}
	if got := len(r.Result.Contradictions); got != len(c.Expect.Contradict) {
		why("expected %d contradiction(s), got %d", len(c.Expect.Contradict), got)
	}
	for _, id := range c.Expect.Contradict {
		found := false
		for _, x := range r.Result.Contradictions {
			found = found || x.Contradicts == id
		}
		if !found {
			why("no contradiction of %s", id)
		}
	}
	if r.Result.OverCap < c.Expect.OverCap {
		why("expected the cap to cut at least %d, it cut %d", c.Expect.OverCap, r.Result.OverCap)
	}
	r.Pass = len(r.Why) == 0
	return r
}

func entries(cs []Candidate) string {
	var s []string
	for _, c := range cs {
		s = append(s, fmt.Sprintf("%q", c.Entry))
	}
	return strings.Join(s, ", ")
}

func srcNote(src []int64) string {
	if len(src) == 0 {
		return ""
	}
	return fmt.Sprintf(" citing %v", src)
}

func matches(got []Candidate, w Want) bool {
	for _, c := range got {
		if c.Path != w.Path {
			continue
		}
		e := strings.ToLower(c.Entry)
		ok := true
		for _, p := range w.All {
			if !strings.Contains(e, strings.ToLower(p)) {
				ok = false
			}
		}
		for _, id := range w.Source {
			cited := false
			for _, s := range c.Source {
				cited = cited || s == id
			}
			ok = ok && cited
		}
		if ok {
			return true
		}
	}
	return false
}
