package memory

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var stampTail = regexp.MustCompile(` <!-- proposal \d{8}-[0-9a-f]{16}, approved \d{4}-\d{2}-\d{2} -->\s*$`)

// factLines finds the lines of a document stamped with proposal id target. A line counts only if
// it is a bullet; text elsewhere that merely mentions the stamp is not a fact.
func factLines(doc string, target string) (idx []int, lines []string) {
	lines = strings.SplitAfter(doc, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "- ") && strings.Contains(ln, "<!-- proposal "+target+",") {
			idx = append(idx, i)
		}
	}
	return idx, lines
}

// lineText is a fact line as the owner reads it: no bullet, no stamp.
func lineText(ln string) string {
	return strings.TrimSpace(strings.TrimPrefix(stampTail.ReplaceAllString(strings.TrimRight(ln, "\r\n"), ""), "- "))
}

// StageRemove records a request to take one fact line out of a document, named by the proposal id
// stamped on it. It never touches the document. The line's text is kept on the proposal: it is
// what the owner is shown, and what the apply must still find.
func (s Store) StageRemove(path, target, rationale string, now time.Time) (Proposal, error) {
	if !idPattern.MatchString(target) {
		return Proposal{}, fmt.Errorf("fact_id %q is not a proposal id (the value after \"proposal\" at the end of a line, e.g. 20261106-0123456789abcdef)", target)
	}
	if strings.TrimSpace(rationale) == "" {
		return Proposal{}, fmt.Errorf("a rationale is required: every memory change records why")
	}
	doc, err := Resolve(s.Root, path)
	if err != nil {
		return Proposal{}, err
	}
	body, err := os.ReadFile(doc)
	if err != nil {
		return Proposal{}, fmt.Errorf("no memory document at %q", path)
	}
	idx, lines := factLines(string(body), target)
	switch len(idx) {
	case 0:
		return Proposal{}, fmt.Errorf("no line in %s carries proposal id %s", path, target)
	case 1:
	default:
		return Proposal{}, fmt.Errorf("%d lines in %s carry proposal id %s; refusing an ambiguous removal", len(idx), path, target)
	}
	text := lineText(lines[idx[0]])
	if text == "" {
		return Proposal{}, fmt.Errorf("the line carrying %s has no text", target)
	}
	id, err := newID(now)
	if err != nil {
		return Proposal{}, err
	}
	p := Proposal{ID: id, Path: path, Remove: target, Line: text, Rationale: rationale, ProposedAt: now.UTC().Format(time.RFC3339)}
	if err := s.write(p); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// applyRemove takes the approved line out. The proposal file is not trusted, as in the add path:
// the target id, the path and the shown text are revalidated, and the line must still read exactly
// as the owner saw it. A line already gone (a crash between the write and the commit) is finished,
// not an error. The file is rewritten in place so its inode and mode survive.
func (s Store) applyRemove(id string, p Proposal, now time.Time) (Proposal, error) {
	if p.Entry != "" || p.Line == "" || !idPattern.MatchString(p.Remove) {
		return Proposal{}, fmt.Errorf("proposal %s is not a well-formed removal", id)
	}
	doc, err := Resolve(s.Root, p.Path)
	if err != nil {
		return Proposal{}, err
	}
	body, err := os.ReadFile(doc)
	if err != nil {
		return Proposal{}, err
	}
	idx, lines := factLines(string(body), p.Remove)
	switch len(idx) {
	case 0:
		// Already removed: fall through to the commit and the filing.
	case 1:
		if lineText(lines[idx[0]]) != p.Line {
			return Proposal{}, fmt.Errorf("the line carrying %s no longer reads as the card showed it", p.Remove)
		}
		rest := append(append([]string{}, lines[:idx[0]]...), lines[idx[0]+1:]...)
		f, err := os.OpenFile(doc, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return Proposal{}, err
		}
		if _, err := f.WriteString(strings.Join(rest, "")); err != nil {
			f.Close()
			return Proposal{}, err
		}
		if err := f.Close(); err != nil {
			return Proposal{}, err
		}
	default:
		return Proposal{}, fmt.Errorf("%d lines carry proposal id %s; refusing an ambiguous removal", len(idx), p.Remove)
	}
	if s.Commit != nil {
		msg := fmt.Sprintf("memory: remove from %s (proposal %s removes %s)", p.Path, id, p.Remove)
		if err := s.Commit(s.Root, doc, msg); err != nil {
			return Proposal{}, fmt.Errorf("removed from %s but not committed: %w", p.Path, err)
		}
	}
	return s.move(id, p, "applied", "approved", now)
}
