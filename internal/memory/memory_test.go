package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidateEntry(t *testing.T) {
	ok := []struct{ in, want string }{
		{"No okra.", "No okra."},
		{"- No okra.", "No okra."},
		{"  No okra.  ", "No okra."},
		{strings.Repeat("भ", 200), strings.Repeat("भ", 200)},
	}
	for _, c := range ok {
		got, err := ValidateEntry(c.in)
		if err != nil || got != c.want {
			t.Errorf("ValidateEntry(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{strings.Repeat("a", 201), "", "   ", "a\nb", "a\rb", "x <!-- y", "x --> y"}
	for _, in := range bad {
		if got, err := ValidateEntry(in); err == nil {
			t.Errorf("ValidateEntry(%q) = %q; want an error", in, got)
		}
	}
}

// newRoot returns a memory root holding taste/food.md.
func newRoot(t *testing.T) (Store, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "taste"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(root, "taste", "food.md")
	if err := os.WriteFile(doc, []byte("# Food\n\n- likes dosa\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	return Store{Root: root}, doc
}

func TestStageWritesAReadableProposal(t *testing.T) {
	s, doc := newRoot(t)
	before, _ := os.ReadFile(doc)
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

	p, err := s.Stage("taste/food", "No okra.", "said so", now)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Root, ".proposals", p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	got, err := s.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("Load = %+v, want %+v", got, p)
	}
	if got.Path != "taste/food" || got.Entry != "No okra." || got.Rationale != "said so" || got.ProposedAt != "2026-10-30T09:00:00Z" {
		t.Errorf("unexpected proposal %+v", got)
	}
	after, _ := os.ReadFile(doc)
	if string(before) != string(after) {
		t.Errorf("document changed by Stage")
	}
}

func TestStageRefusesBadInput(t *testing.T) {
	s, _ := newRoot(t)
	now := time.Now()
	cases := []struct{ name, path, entry, why string }{
		{"traversal", "../x", "No okra.", "r"},
		{"absolute", "/etc/passwd", "No okra.", "r"},
		{"newline", "taste/food", "a\nb", "r"},
		{"no rationale", "taste/food", "No okra.", ""},
	}
	for _, c := range cases {
		if _, err := s.Stage(c.path, c.entry, c.why, now); err == nil {
			t.Errorf("%s: want an error", c.name)
		}
		files, _ := filepath.Glob(filepath.Join(s.Root, ".proposals", "*"))
		if len(files) != 0 {
			t.Errorf("%s: left files behind: %v", c.name, files)
		}
	}
}

func TestLoadRefusesAnIDThatIsAPath(t *testing.T) {
	s, _ := newRoot(t)
	for _, id := range []string{"../../etc/passwd", "20261030-zz"} {
		if _, err := s.Load(id); err == nil {
			t.Errorf("Load(%q): want an error", id)
		}
	}
}

func TestPendingSplitsLegacyFromReady(t *testing.T) {
	s, _ := newRoot(t)
	if _, err := s.Stage("taste/food", "No okra.", "said so", time.Now()); err != nil {
		t.Fatal(err)
	}
	const legacyID = "20261029-688f393323e57be8"
	raw := `{"proposal_id":"` + legacyID + `","path":"taste/food","diff":"Add to dislikes list","rationale":"x","proposed_at":"2026-10-29T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(s.Root, ".proposals", legacyID+".json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	ready, legacy, _, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || len(legacy) != 1 || legacy[0] != legacyID {
		t.Fatalf("ready=%d legacy=%v", len(ready), legacy)
	}

	if err := s.MoveLegacy(legacyID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".proposals", "legacy", legacyID+".json")); err != nil {
		t.Errorf("not moved: %v", err)
	}
	fi, _ := os.Stat(filepath.Join(s.Root, ".proposals", "legacy"))
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("legacy dir mode = %v, want 0755", fi.Mode().Perm())
	}
	ready, legacy, _, err = s.Pending()
	if err != nil || len(legacy) != 0 || len(ready) != 1 {
		t.Errorf("after move: ready=%d legacy=%v err=%v", len(ready), legacy, err)
	}
}

var t1030 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)

func line(id, entry string) string {
	return "- " + entry + " <!-- proposal " + id + ", approved 2026-10-30 -->\n"
}

func stage(t *testing.T, s Store, path, entry string) Proposal {
	t.Helper()
	p, err := s.Stage(path, entry, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rewriteProposal(t *testing.T, s Store, id string, mut func(*Proposal)) {
	t.Helper()
	p, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	mut(&p)
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(s.Root, ".proposals", id+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyAppendsExactlyOneLineAndKeepsTheInode(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	doc := filepath.Join(root, "taste", "food.md")
	if err := os.WriteFile(doc, []byte("# Food\n\nNo coriander."), 0o664); err != nil {
		t.Fatal(err)
	}
	ino := func() uint64 {
		fi, err := os.Stat(doc)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	before := ino()
	p, err := s.Stage("taste/food", "No okra.", "said so", t1030)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ApplyApproved(p.ID, t1030)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "approved" || got.DecidedAt != "2026-10-30T10:00:00Z" {
		t.Errorf("decision fields = %q %q", got.Decision, got.DecidedAt)
	}
	if want := "# Food\n\nNo coriander.\n" + line(p.ID, "No okra."); readFile(t, doc) != want {
		t.Errorf("doc = %q; want %q", readFile(t, doc), want)
	}
	if ino() != before {
		t.Error("inode changed")
	}
	if _, err := os.Stat(filepath.Join(root, ".proposals", "applied", p.ID+".json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".proposals", p.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("pending file still there: %v", err)
	}
}

func TestApplyOnAnEmptyDocument(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	doc := filepath.Join(root, "taste", "food.md")
	if err := os.WriteFile(doc, nil, 0o664); err != nil {
		t.Fatal(err)
	}
	p := stage(t, s, "taste/food", "No okra.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, doc), line(p.ID, "No okra."); got != want {
		t.Errorf("doc = %q; want %q", got, want)
	}
}

func TestApplyWritesSpecialCharactersVerbatim(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	entry := `No "masala" & no <b>okra</b>`
	p, err := s.Stage("taste/food", entry, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "taste", "food.md")), line(p.ID, entry)) {
		t.Error("entry not written verbatim")
	}
}

func TestApplyCreatesANewTitledDocument(t *testing.T) {
	// The umask would otherwise yield 0644; pin it so the test proves the chmod.
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	s, _ := newRoot(t)
	root := s.Root
	p, err := s.Stage("taste/drinks", "No tea.", "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(root, "taste", "drinks.md")
	if got, want := readFile(t, doc), "# Drinks\n\n"+line(p.ID, "No tea."); got != want {
		t.Errorf("doc = %q; want %q", got, want)
	}
	fi, _ := os.Stat(doc)
	if fi.Mode().Perm() != 0o664 {
		t.Errorf("mode = %o; want 664", fi.Mode().Perm())
	}
}

func TestApplyIsIdempotentAfterACrash(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	doc := filepath.Join(root, "taste", "food.md")
	p := stage(t, s, "taste/food", "No okra.")
	f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line(p.ID, "No okra.")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, doc), p.ID); n != 1 {
		t.Errorf("id appears %d times; want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root, ".proposals", "applied", p.ID+".json")); err != nil {
		t.Error(err)
	}
}

func TestTwoApprovalsLandInOrder(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	a := stage(t, s, "taste/food", "Entry A.")
	b := stage(t, s, "taste/food", "Entry B.")
	for _, id := range []string{b.ID, a.ID} {
		if _, err := s.ApplyApproved(id, t1030); err != nil {
			t.Fatal(err)
		}
	}
	got := readFile(t, filepath.Join(root, "taste", "food.md"))
	if ib, ia := strings.Index(got, "Entry B."), strings.Index(got, "Entry A."); ib < 0 || ia < 0 || ib > ia {
		t.Errorf("order wrong: %q", got)
	}
}

func TestApplyRefusesAProposalEditedToEscape(t *testing.T) {
	rewrite := func(t *testing.T, s Store, root, id string, mut func(*Proposal)) {
		p, err := s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		mut(&p)
		b, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(root, ".proposals", id+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, s Store, root string, p *Proposal){
		"path":        func(t *testing.T, s Store, root string, p *Proposal) { p.Path = "../outside" },
		"proposal_id": func(t *testing.T, s Store, root string, p *Proposal) { p.ID = "../outside" },
		"entry":       func(t *testing.T, s Store, root string, p *Proposal) { p.Entry = "x\ny" },
		"symlink": func(t *testing.T, s Store, root string, p *Proposal) {
			out := t.TempDir()
			if err := os.RemoveAll(filepath.Join(root, "taste")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(out, filepath.Join(root, "taste")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newRoot(t)
			root := s.Root
			p, err := s.Stage("taste/food", "No okra.", "r", t1030)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(filepath.Dir(root), "outside.md")
			rewrite(t, s, root, p.ID, func(q *Proposal) { mut(t, s, root, q) })
			if _, err := s.ApplyApproved(p.ID, t1030); err == nil {
				t.Fatal("want an error")
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Errorf("wrote outside the root: %v", err)
			}
			if name == "symlink" {
				if ents, _ := os.ReadDir(filepath.Join(root, "taste")); len(ents) != 0 {
					t.Errorf("wrote through the symlink: %v", ents)
				}
			}
			if _, err := os.Stat(filepath.Join(root, ".proposals", p.ID+".json")); err != nil {
				t.Errorf("proposal not left pending: %v", err)
			}
		})
	}
}

func TestApplyOnAMissingProposalErrors(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	doc := filepath.Join(root, "taste", "food.md")
	before := readFile(t, doc)
	p := stage(t, s, "taste/food", "No okra.")
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err == nil {
		t.Fatal("want an error")
	}
	if readFile(t, doc) != before {
		t.Error("document changed")
	}
}

func TestRejectLeavesTheDocumentByteIdentical(t *testing.T) {
	s, _ := newRoot(t)
	root := s.Root
	doc := filepath.Join(root, "taste", "food.md")
	before := readFile(t, doc)
	p := stage(t, s, "taste/food", "No okra.")
	got, err := s.Reject(p.ID, t1030)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "rejected" || got.DecidedAt != "2026-10-30T10:00:00Z" {
		t.Errorf("decision fields = %q %q", got.Decision, got.DecidedAt)
	}
	if readFile(t, doc) != before {
		t.Error("document changed")
	}
	if _, err := os.Stat(filepath.Join(root, ".proposals", "rejected", p.ID+".json")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".proposals", p.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("pending file still there: %v", err)
	}
}

func TestDecisionsRefuseAForgedProposalID(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(Store, string) (Proposal, error)
	}{
		{"apply", func(s Store, id string) (Proposal, error) { return s.ApplyApproved(id, t1030) }},
		{"reject", func(s Store, id string) (Proposal, error) { return s.Reject(id, t1030) }},
	} {
		for _, forged := range []string{"../outside", ""} {
			t.Run(tc.name+forged, func(t *testing.T) {
				s, doc := newRoot(t)
				before := readFile(t, doc)
				p := stage(t, s, "taste/food", "No okra.")
				other := stage(t, s, "taste/food", "Other.")
				if forged == "" {
					forged = other.ID // another pending proposal's id
				}
				rewriteProposal(t, s, p.ID, func(q *Proposal) { q.ID = forged })
				if _, err := tc.do(s, p.ID); err == nil {
					t.Fatal("want an error")
				}
				// A forged "../outside" is joined under .proposals/applied or
				// .proposals/rejected, so it lands in .proposals itself, not
				// above the root. Stat every place the forged id could reach.
				for _, hit := range []string{
					filepath.Join(s.Root, ".proposals", "applied", "..", "outside.json"),
					filepath.Join(s.Root, ".proposals", "rejected", "..", "outside.json"),
					filepath.Join(s.Root, "outside.json"),
					filepath.Join(filepath.Dir(s.Root), "outside.json"),
				} {
					if _, err := os.Stat(hit); !os.IsNotExist(err) {
						t.Errorf("forged id wrote %s: %v", hit, err)
					}
				}
				if readFile(t, doc) != before {
					t.Error("document changed")
				}
				for _, id := range []string{p.ID, other.ID} {
					if _, err := os.Stat(filepath.Join(s.Root, ".proposals", id+".json")); err != nil {
						t.Errorf("proposal %s not left pending: %v", id, err)
					}
				}
			})
		}
	}
}

func TestApplyTitleIsRuneSafe(t *testing.T) {
	s, _ := newRoot(t)
	p := stage(t, s, "taste/éclair", "No cream.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(s.Root, "taste", "éclair.md")); !strings.HasPrefix(got, "# Éclair\n\n") {
		t.Errorf("doc = %q", got)
	}
}

// A legacy proposal was written 0600 by the gateway's uid. Shelving it must
// also make it readable, or the operator still cannot see what it said.
func TestMoveLegacyLeavesTheFileReadable(t *testing.T) {
	s, _ := newRoot(t)
	if err := os.MkdirAll(filepath.Join(s.Root, ".proposals"), 0o755); err != nil {
		t.Fatal(err)
	}
	const legacyID = "20261029-688f393323e57be8"
	raw := `{"proposal_id":"` + legacyID + `","path":"taste/food","diff":"x","rationale":"x"}`
	src := filepath.Join(s.Root, ".proposals", legacyID+".json")
	if err := os.WriteFile(src, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.MoveLegacy(legacyID); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Root, ".proposals", "legacy", legacyID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("shelved proposal mode = %v, want 0644", fi.Mode().Perm())
	}
}

// A crash between the append and the move leaves the line in the document
// and the proposal pending. Rejecting it then would file as rejected a line
// that is in memory, so Reject refuses and approve finishes the filing.
func TestRejectRefusesAProposalAlreadyWritten(t *testing.T) {
	s, doc := newRoot(t)
	p := stage(t, s, "taste/food", "No okra.")
	f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line(p.ID, "No okra.")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	before := readFile(t, doc)

	if _, err := s.Reject(p.ID, t1030); !errors.Is(err, ErrAlreadyWritten) {
		t.Fatalf("Reject = %v, want ErrAlreadyWritten", err)
	}
	if readFile(t, doc) != before {
		t.Error("document changed")
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".proposals", p.ID+".json")); err != nil {
		t.Errorf("proposal not left pending: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".proposals", "rejected", p.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("filed as rejected: %v", err)
	}

	// Approve is the way out: it sees the line, does not write it twice, and files it.
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, doc), p.ID); n != 1 {
		t.Errorf("id appears %d times; want 1", n)
	}
}

// One unreadable proposal must not hide the others: a restart would then
// re-post no card and shelve no legacy file at all.
func TestPendingSkipsACorruptProposal(t *testing.T) {
	s, _ := newRoot(t)
	good := stage(t, s, "taste/food", "No okra.")
	const legacyID = "20261029-688f393323e57be8"
	const corruptID = "20261029-0000000000000000"
	write := func(id, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.Root, ".proposals", id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(legacyID, `{"proposal_id":"`+legacyID+`","path":"taste/food","diff":"x","rationale":"x"}`)
	write(corruptID, `{not json`)

	ready, legacy, skipped, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != good.ID {
		t.Errorf("ready = %v, want only %s", ready, good.ID)
	}
	if len(legacy) != 1 || legacy[0] != legacyID {
		t.Errorf("legacy = %v", legacy)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), corruptID) {
		t.Errorf("skipped = %v, want the corrupt file named", skipped)
	}
}

// In production .proposals/ is created by dev.sh, owned by the operator, and
// carries the gateway's ACL. The gateway is not its owner, so chmod on it is
// EPERM — and if it were permitted, forcing 0755 would rewrite the ACL mask to
// r-x and take write away from the gateway itself. A directory that already
// exists is not ours to re-mode. Tests run as the owner, so the EPERM cannot be
// reproduced here; an unchanged mode is the observable proof that no chmod ran.
func TestExistingProposalDirectoriesKeepTheirMode(t *testing.T) {
	s, _ := newRoot(t)
	for _, d := range []string{s.dir(), filepath.Join(s.dir(), "applied"), filepath.Join(s.dir(), "legacy")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}

	p := stage(t, s, "taste/food", "No okra.")
	if _, err := s.ApplyApproved(p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(s.dir(), "20261029-688f393323e57be8.json")
	if err := os.WriteFile(legacy, []byte(`{"proposal_id":"20261029-688f393323e57be8","path":"taste/food","diff":"x","rationale":"x","proposed_at":"2026-10-29T16:46:51Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveLegacy("20261029-688f393323e57be8"); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{s.dir(), filepath.Join(s.dir(), "applied"), filepath.Join(s.dir(), "legacy")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o775 {
			t.Errorf("%s mode = %o, want 0775 left alone: an existing directory must not be chmod'd", d, got)
		}
	}
}
