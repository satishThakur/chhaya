package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	idDosa  = "20261030-aaaaaaaaaaaaaaaa"
	idOkra  = "20261030-bbbbbbbbbbbbbbbb"
	idStale = "20261030-cccccccccccccccc"
)

// removeRoot is a document with three fact lines, so a removal can be shown to take one and
// leave the others.
func removeRoot(t *testing.T) (Store, string, *[]string) {
	t.Helper()
	s, doc := newRoot(t)
	body := "# Food\n\n" + line(idDosa, "Likes dosa.") + line(idOkra, "No okra.") + line(idStale, "Eats out on Fridays.")
	if err := os.WriteFile(doc, []byte(body), 0o664); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	s.Commit = func(root, d, msg string) error { msgs = append(msgs, msg); return nil }
	return s, doc, &msgs
}

func TestStageRemoveRecordsTheTargetAndTouchesNothing(t *testing.T) {
	s, doc, _ := removeRoot(t)
	before := readFile(t, doc)
	p, err := s.StageRemove("taste/food", idOkra, "duplicate of another line", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if p.Remove != idOkra || p.Line != "No okra." || p.Entry != "" {
		t.Fatalf("proposal: %+v", p)
	}
	if readFile(t, doc) != before {
		t.Fatal("staging changed the document")
	}
	got, err := s.Load(p.ID)
	if err != nil || got.Remove != idOkra || got.Line != "No okra." {
		t.Fatalf("loaded %+v, %v", got, err)
	}
}

func TestStageRemoveRefusesWhatItCannotName(t *testing.T) {
	s, doc, _ := removeRoot(t)
	before := readFile(t, doc)
	for name, c := range map[string]struct{ path, id, why string }{
		"no such fact":     {"taste/food", "20261030-dddddddddddddddd", "r"},
		"id is a path":     {"taste/food", "../../etc/passwd", "r"},
		"id is a fragment": {"taste/food", "20261030", "r"},
		"no rationale":     {"taste/food", idOkra, "  "},
		"path escapes":     {"../outside", idOkra, "r"},
		"dot directory":    {".tests/probes", idOkra, "r"},
		"no such document": {"taste/drinks", idOkra, "r"},
		"empty id":         {"taste/food", "", "r"},
	} {
		if _, err := s.StageRemove(c.path, c.id, c.why, t1030); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	if readFile(t, doc) != before {
		t.Fatal("a refused staging changed the document")
	}
	if ready, _, _, _ := s.Pending(); len(ready) != 0 {
		t.Fatalf("a refused staging left %d proposal(s) behind", len(ready))
	}
}

func TestStageRemoveRefusesAnIDThatIsOnTwoLines(t *testing.T) {
	s, doc, _ := removeRoot(t)
	body := readFile(t, doc) + line(idOkra, "No okra, really.")
	if err := os.WriteFile(doc, []byte(body), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StageRemove("taste/food", idOkra, "r", t1030); err == nil {
		t.Fatal("two lines carry the id: the removal would be ambiguous")
	}
}

func TestARemovalIsReadyToCardNotLegacy(t *testing.T) {
	s, _, _ := removeRoot(t)
	p, err := s.StageRemove("taste/food", idOkra, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	ready, legacy, _, err := s.Pending()
	if err != nil || len(legacy) != 0 || len(ready) != 1 || ready[0].ID != p.ID {
		t.Fatalf("ready %+v legacy %v err %v", ready, legacy, err)
	}
}

func TestApplyRemovesExactlyOneLineKeepsTheInodeAndCommits(t *testing.T) {
	s, doc, msgs := removeRoot(t)
	ino := func() uint64 {
		fi, err := os.Stat(doc)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	before := ino()
	p, err := s.StageRemove("taste/food", idOkra, "duplicate", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	want := "# Food\n\n" + line(idDosa, "Likes dosa.") + line(idStale, "Eats out on Fridays.")
	if got := readFile(t, doc); got != want {
		t.Fatalf("document is now:\n%s\nwant:\n%s", got, want)
	}
	if ino() != before {
		t.Fatal("the removal replaced the file instead of rewriting it")
	}
	if len(*msgs) != 1 || !strings.Contains((*msgs)[0], "remove from taste/food") || !strings.Contains((*msgs)[0], p.ID) || !strings.Contains((*msgs)[0], idOkra) {
		t.Fatalf("commit messages: %q", *msgs)
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".proposals", "applied", p.ID+".json")); err != nil {
		t.Fatalf("the proposal was not filed as applied: %v", err)
	}
	if _, err := s.Load(p.ID); err == nil {
		t.Fatal("the proposal is still pending")
	}
}

func TestApplyRefusesWhenTheLineChangedSinceTheCard(t *testing.T) {
	s, doc, msgs := removeRoot(t)
	p, err := s.StageRemove("taste/food", idOkra, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(readFile(t, doc), "No okra.", "No okra or brinjal.", 1)
	if err := os.WriteFile(doc, []byte(changed), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err == nil {
		t.Fatal("what the owner approved was a different line")
	}
	if readFile(t, doc) != changed || len(*msgs) != 0 {
		t.Fatal("a refused removal wrote or committed")
	}
	if _, err := s.Load(p.ID); err != nil {
		t.Fatalf("the proposal must stay pending: %v", err)
	}
}

func TestApplyRemoveIsIdempotentAfterACrash(t *testing.T) {
	s, doc, msgs := removeRoot(t)
	p, err := s.StageRemove("taste/food", idOkra, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	// The line was removed, the process died before the commit and the move.
	gone := strings.Replace(readFile(t, doc), line(idOkra, "No okra."), "", 1)
	if err := os.WriteFile(doc, []byte(gone), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if readFile(t, doc) != gone || len(*msgs) != 1 {
		t.Fatalf("a retry must finish the commit and nothing else; doc:\n%s msgs %q", readFile(t, doc), *msgs)
	}
}

func TestApplyRemoveRefusesAProposalEditedToTargetAnotherLine(t *testing.T) {
	s, doc, msgs := removeRoot(t)
	before := readFile(t, doc)
	p, err := s.StageRemove("taste/food", idOkra, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Proposal){
		"other target":        func(p *Proposal) { p.Remove = idDosa },
		"target is a path":    func(p *Proposal) { p.Remove = "../../x" },
		"path escapes":        func(p *Proposal) { p.Path = "../outside" },
		"entry added too":     func(p *Proposal) { p.Entry = "Likes everything." },
		"snapshot erased":     func(p *Proposal) { p.Line = "" },
		"snapshot of another": func(p *Proposal) { p.Line = "Likes dosa." },
	} {
		rewriteProposal(t, s, p.ID, func(q *Proposal) { *q = p; mut(q) })
		if _, err := s.ApplyApproved(p.ID, t1030); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	if readFile(t, doc) != before || len(*msgs) != 0 {
		t.Fatal("a forged removal changed the document")
	}
}

func TestRejectingARemovalLeavesTheDocumentByteIdentical(t *testing.T) {
	s, doc, msgs := removeRoot(t)
	before := readFile(t, doc)
	p, err := s.StageRemove("taste/food", idOkra, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reject(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if readFile(t, doc) != before || len(*msgs) != 0 {
		t.Fatal("a rejection wrote")
	}
}

func TestRemovingTheLastLineLeavesTheTitle(t *testing.T) {
	s, doc := newRoot(t)
	if err := os.WriteFile(doc, []byte("# Food\n\n"+line(idDosa, "Likes dosa.")), 0o664); err != nil {
		t.Fatal(err)
	}
	p, err := s.StageRemove("taste/food", idDosa, "r", t1030)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, doc); got != "# Food\n\n" {
		t.Fatalf("document is %q", got)
	}
}
