package memory

import (
	"os"
	"path/filepath"
	"strings"
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
