package memory

import (
	"encoding/json"
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

	ready, legacy, err := s.Pending()
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
	ready, legacy, err = s.Pending()
	if err != nil || len(legacy) != 0 || len(ready) != 1 {
		t.Errorf("after move: ready=%d legacy=%v err=%v", len(ready), legacy, err)
	}
}

var t1030 = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)

func line(id, entry string) string {
	return "- " + entry + " <!-- proposal " + id + ", approved 2026-10-30 -->\n"
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
	p, _ := s.Stage("taste/food", "No okra.", "r", t1030)
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
	p, _ := s.Stage("taste/food", "No okra.", "r", t1030)
	f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line(p.ID, "No okra."))
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
	a, _ := s.Stage("taste/food", "Entry A.", "r", t1030)
	b, _ := s.Stage("taste/food", "Entry B.", "r", t1030)
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
		"path":  func(t *testing.T, s Store, root string, p *Proposal) { p.Path = "../outside" },
		"entry": func(t *testing.T, s Store, root string, p *Proposal) { p.Entry = "x\ny" },
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
	p, _ := s.Stage("taste/food", "No okra.", "r", t1030)
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
	p, _ := s.Stage("taste/food", "No okra.", "r", t1030)
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
