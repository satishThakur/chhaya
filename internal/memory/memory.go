// Package memory holds the Personal memory proposal store: staging a proposal,
// validating the entry it carries, and resolving a document path inside the
// memory root. It is pure filesystem; the tools and the approver build on it.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxEntryRunes = 200

// ErrAlreadyWritten is Reject's refusal for a proposal whose line is already
// in its document: a crash between the append and the move left it pending.
// Approve finishes the filing; reject would file a written line as rejected.
var ErrAlreadyWritten = errors.New("already written; approve to finish filing it")

// Resolve resolves a document path inside the memory root and refuses
// anything that leaves it, including a path whose final component does not
// exist yet — the case a write would hit.
func Resolve(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path must be relative to the memory root")
	}
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}

	// Reject traversal on the cleaned path BEFORE appending ".md". Clean turns
	// "taste/food/../../.." into "..", and ".."+".md" fuses into the ordinary
	// filename "...md" — which lands inside the root and silently looks safe.
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}

	p := filepath.Join(absRoot, clean+".md")

	// The parent must exist and resolve inside the root. Resolving only the
	// full path skips the check whenever the final component does not exist
	// yet, which is exactly the case a write would hit.
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", fmt.Errorf("path %q: %w", rel, err)
	}
	if !containedIn(absRoot, parent) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}

	final := filepath.Join(parent, filepath.Base(p))

	// If the final component already exists it may itself be a symlink out.
	if resolved, err := filepath.EvalSymlinks(final); err == nil && !containedIn(absRoot, resolved) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}
	return final, nil
}

func containedIn(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// ValidateEntry normalises a proposed memory line and refuses one that could
// not be written as a single line or that could break out of the provenance
// comment appended after it.
func ValidateEntry(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "- "))
	if s == "" {
		return "", fmt.Errorf("entry is required")
	}
	if strings.ContainsAny(s, "\n\r") {
		return "", fmt.Errorf("entry must be a single line")
	}
	// Runes, not bytes: the limit must not penalise non-Latin scripts.
	if n := utf8.RuneCountInString(s); n > maxEntryRunes {
		return "", fmt.Errorf("entry is %d characters; the limit is %d", n, maxEntryRunes)
	}
	if strings.Contains(s, "<!--") || strings.Contains(s, "-->") {
		return "", fmt.Errorf("entry must not contain an HTML comment marker")
	}
	return s, nil
}

// Proposal is one staged change. Diff exists only so files written before
// proposals carried an entry can be recognised and set aside.
type Proposal struct {
	ID         string `json:"proposal_id"`
	Path       string `json:"path"`
	Entry      string `json:"entry,omitempty"`
	Diff       string `json:"diff,omitempty"`
	Rationale  string `json:"rationale"`
	ProposedAt string `json:"proposed_at"`
	DecidedAt  string `json:"decided_at,omitempty"`
	Decision   string `json:"decision,omitempty"`
}

// Store is the proposal queue under <Root>/.proposals.
type Store struct{ Root string }

var idPattern = regexp.MustCompile(`^\d{8}-[0-9a-f]{16}$`)

func (s Store) dir() string { return filepath.Join(s.Root, ".proposals") }

func newID(now time.Time) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102") + "-" + hex.EncodeToString(b), nil
}

// mkdir creates dir and forces its mode, because Mkdir honours the umask. It
// re-modes only a directory it just created. An existing one — .proposals/
// itself, made by dev.sh — is owned by the operator and carries the gateway's
// ACL: chmod there is EPERM for the gateway, and if it were allowed, forcing
// 0755 would rewrite the ACL mask to r-x and revoke the gateway's own write.
func mkdir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return os.Chmod(dir, 0o755)
}

// Stage validates and records a proposal. It never touches the document.
func (s Store) Stage(path, entry, rationale string, now time.Time) (Proposal, error) {
	entry, err := ValidateEntry(entry)
	if err != nil {
		return Proposal{}, err
	}
	if strings.TrimSpace(rationale) == "" {
		return Proposal{}, fmt.Errorf("a rationale is required: every memory entry records its provenance")
	}
	if _, err := Resolve(s.Root, path); err != nil {
		return Proposal{}, err
	}
	id, err := newID(now)
	if err != nil {
		return Proposal{}, err
	}
	p := Proposal{ID: id, Path: path, Entry: entry, Rationale: rationale, ProposedAt: now.UTC().Format(time.RFC3339)}
	body, err := json.Marshal(p)
	if err != nil {
		return Proposal{}, err
	}
	if err := mkdir(s.dir()); err != nil {
		return Proposal{}, err
	}
	file := filepath.Join(s.dir(), id+".json")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		return Proposal{}, err
	}
	// WriteFile honours the umask; the approver may run as another user.
	if err := os.Chmod(file, 0o644); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// Load reads one proposal. The id is checked against the generated format
// first, so it can never be used as a path.
func (s Store) Load(id string) (Proposal, error) {
	if !idPattern.MatchString(id) {
		return Proposal{}, fmt.Errorf("invalid proposal id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.dir(), id+".json"))
	if err != nil {
		return Proposal{}, err
	}
	var p Proposal
	if err := json.Unmarshal(b, &p); err != nil {
		return Proposal{}, fmt.Errorf("proposal %s: %w", id, err)
	}
	// The file is untrusted: an id that differs from the one asked for would
	// steer the move out of .proposals or onto another proposal.
	if p.ID != id {
		return Proposal{}, fmt.Errorf("proposal %s: file carries id %q", id, p.ID)
	}
	return p, nil
}

// Remove deletes a proposal file.
func (s Store) Remove(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid proposal id %q", id)
	}
	return os.Remove(filepath.Join(s.dir(), id+".json"))
}

// Pending lists the proposals sitting directly in .proposals, sorted by ID.
// A file with an entry is ready to card; one without predates entries and is
// reported by ID in legacy. A file that will not load is reported in skipped
// and does not stop the rest: one corrupt file must not leave every other
// proposal without a card. err is only for a directory that cannot be read.
func (s Store) Pending() (ready []Proposal, legacy []string, skipped []error, err error) {
	files, err := os.ReadDir(s.dir())
	if os.IsNotExist(err) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	for _, f := range files {
		name := f.Name()
		id := strings.TrimSuffix(name, ".json")
		if f.IsDir() || id == name || !idPattern.MatchString(id) {
			continue
		}
		p, err := s.Load(id)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if p.Entry == "" {
			legacy = append(legacy, id)
		} else {
			ready = append(ready, p)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	sort.Strings(legacy)
	return ready, legacy, skipped, nil
}

// MoveLegacy sets a pre-entry proposal aside under .proposals/legacy.
func (s Store) MoveLegacy(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid proposal id %q", id)
	}
	dir := filepath.Join(s.dir(), "legacy")
	if err := mkdir(dir); err != nil {
		return err
	}
	dst := filepath.Join(dir, id+".json")
	if err := os.Rename(filepath.Join(s.dir(), id+".json"), dst); err != nil {
		return err
	}
	// Legacy files were written 0600 by the gateway's uid; a rename keeps the
	// mode, and the operator could still not read what was shelved.
	return os.Chmod(dst, 0o644)
}

// ApplyApproved appends an approved proposal's entry to its document and
// moves the proposal to .proposals/applied. It is the only function that
// writes a memory document. The file is never trusted: the entry and path
// are revalidated, so a proposal edited on disk cannot escape the root or
// break out of its line. Any error before the move leaves the proposal
// pending.
func (s Store) ApplyApproved(id string, now time.Time) (Proposal, error) {
	p, err := s.Load(id)
	if err != nil {
		return Proposal{}, err
	}
	entry, err := ValidateEntry(p.Entry)
	if err != nil {
		return Proposal{}, err
	}
	doc, err := Resolve(s.Root, p.Path)
	if err != nil {
		return Proposal{}, err
	}
	if err := appendEntry(doc, id, entry, now); err != nil {
		return Proposal{}, err
	}
	return s.move(id, p, "applied", "approved", now)
}

// Reject moves a proposal to .proposals/rejected. It reads the document, never
// writes it: a proposal whose line is already there is refused with
// ErrAlreadyWritten. A path that does not resolve is not an error here — no
// apply could have written through it, and refusing would leave a forged path
// with no way off the queue.
func (s Store) Reject(id string, now time.Time) (Proposal, error) {
	p, err := s.Load(id)
	if err != nil {
		return Proposal{}, err
	}
	if doc, err := Resolve(s.Root, p.Path); err == nil {
		existing, err := os.ReadFile(doc)
		switch {
		case err == nil:
			if written(existing, id) {
				return Proposal{}, ErrAlreadyWritten
			}
		case !os.IsNotExist(err):
			// Fail closed: a document that cannot be read cannot be shown
			// not to hold the line.
			return Proposal{}, err
		}
	}
	return s.move(id, p, "rejected", "rejected", now)
}

// written reports whether a document already holds proposal id's line.
func written(doc []byte, id string) bool {
	return strings.Contains(string(doc), "<!-- proposal "+id+",")
}

// appendEntry writes the provenance-stamped line with O_APPEND, never
// temp-file-and-rename, so the inode and any concurrent writer survive.
func appendEntry(doc, id, entry string, now time.Time) error {
	stamped := fmt.Sprintf("- %s <!-- proposal %s, approved %s -->\n", entry, id, now.UTC().Format("2006-01-02"))

	existing, err := os.ReadFile(doc)
	switch {
	case os.IsNotExist(err):
		title := filepath.Base(strings.TrimSuffix(doc, ".md"))
		r, n := utf8.DecodeRuneInString(title)
		title = string(unicode.ToUpper(r)) + title[n:]
		f, err := os.OpenFile(doc, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
		if err != nil {
			return err
		}
		// A document left empty or headerless would take the next append with
		// no title, so any failure after the create removes it.
		fail := func(err error) error {
			f.Close()
			os.Remove(doc)
			return err
		}
		// The umask would otherwise leave 0644, and the agent's worker is another user.
		if err := os.Chmod(doc, 0o664); err != nil {
			return fail(err)
		}
		if _, err := f.WriteString("# " + title + "\n\n" + stamped); err != nil {
			return fail(err)
		}
		if err := f.Close(); err != nil {
			os.Remove(doc)
			return err
		}
		return nil
	case err != nil:
		return err
	}

	// A crash after the append and before the move leaves the line behind;
	// a retry must not write it twice.
	if written(existing, id) {
		return nil
	}
	// The newline and the line go in one Write so a crash cannot leave a
	// bare newline behind.
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		stamped = "\n" + stamped
	}
	f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(stamped); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// move records the decision in sub/<id>.json and removes the pending file.
func (s Store) move(id string, p Proposal, sub, decision string, now time.Time) (Proposal, error) {
	p.Decision = decision
	p.DecidedAt = now.UTC().Format(time.RFC3339)
	body, err := json.Marshal(p)
	if err != nil {
		return Proposal{}, err
	}
	dir := filepath.Join(s.dir(), sub)
	if err := mkdir(dir); err != nil {
		return Proposal{}, err
	}
	file := filepath.Join(dir, id+".json")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		return Proposal{}, err
	}
	if err := os.Chmod(file, 0o644); err != nil {
		return Proposal{}, err
	}
	if err := s.Remove(id); err != nil {
		return Proposal{}, err
	}
	return p, nil
}
