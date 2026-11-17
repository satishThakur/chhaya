package memtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	idA = "20261106-3f609264ada59fd2"
	idB = "20261110-7b17ca3bbb0d259e"
)

func line(text, id string) string {
	return "- " + text + " <!-- proposal " + id + ", approved 2026-11-10 -->\n"
}

func memory(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "interests.md", "# Interests\n\n"+line("Loves reading books.", idA)+line("Enjoys Osho's discourses.", idB))
	return root
}

func problems(r Report, level string) []string {
	var out []string
	for _, p := range r.Problems {
		if p.Level == level {
			out = append(out, p.Msg)
		}
	}
	return out
}

func TestFactsAreParsedWithTheirProposalID(t *testing.T) {
	r, err := Check(memory(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Facts) != 2 || r.Facts[0].ID != idA || r.Facts[0].Path != "interests" || r.Facts[1].ID != idB {
		t.Fatalf("facts: %+v", r.Facts)
	}
}

func TestALineWithNoIDFails(t *testing.T) {
	root := memory(t)
	write(t, root, "location.md", "# Location\n\n- Lives in Pune.\n")
	r, _ := Check(root)
	if f := problems(r, "fail"); len(f) != 1 || !strings.Contains(f[0], "location") {
		t.Fatalf("want one failure naming location, got %v", f)
	}
}

func TestTwoLinesSharingAnIDFail(t *testing.T) {
	root := memory(t)
	write(t, root, "family.md", "# Family\n\n"+line("Has a sister.", idA))
	r, _ := Check(root)
	if f := problems(r, "fail"); len(f) != 1 || !strings.Contains(f[0], idA) {
		t.Fatalf("want one duplicate-id failure, got %v", f)
	}
}

func TestDotDirectoriesAreNotMemory(t *testing.T) {
	root := memory(t)
	write(t, root, ".proposals/x.md", "- not memory\n")
	write(t, root, ".tests/notes.md", "- not memory either\n")
	r, _ := Check(root)
	if len(r.Facts) != 2 || len(problems(r, "fail")) != 0 {
		t.Fatalf("facts %d, fails %v", len(r.Facts), problems(r, "fail"))
	}
}

func TestProbeMustNameAnExistingFact(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/probes.json", `[
	 {"id":"p1","fact":"`+idA+`","kind":"direct","question":"What does the owner love?","expected":"books"},
	 {"id":"p2","fact":"20261101-0000000000000000","kind":"direct","question":"q","expected":"e"}]`)
	r, _ := Check(root)
	f := problems(r, "fail")
	if len(f) != 1 || !strings.Contains(f[0], "p2") {
		t.Fatalf("want only p2 failing, got %v", f)
	}
}

func TestProbeShapeIsChecked(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/probes.json", `[
	 {"id":"p1","fact":"`+idA+`","kind":"sideways","question":"q","expected":"e"},
	 {"id":"p1","fact":"`+idA+`","kind":"direct","question":"","expected":"e"},
	 {"id":"n1","kind":"negative","question":"Does the owner own a boat?","expected":"nothing"}]`)
	r, _ := Check(root)
	if f := problems(r, "fail"); len(f) != 3 { // bad kind, empty question, duplicate id; the negative probe is fine
		t.Fatalf("want 3 failures, got %v", f)
	}
}

func TestFactsWithoutAProbeAreReportedNotFailed(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/probes.json", `[{"id":"p1","fact":"`+idA+`","kind":"direct","question":"q","expected":"e"}]`)
	r, _ := Check(root)
	if len(problems(r, "fail")) != 0 {
		t.Fatalf("a missing probe must not fail: %v", problems(r, "fail"))
	}
	if w := problems(r, "warn"); len(w) != 1 || !strings.Contains(w[0], idB) {
		t.Fatalf("want one warning naming %s, got %v", idB, w)
	}
}

func TestGoldenSetShape(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/golden.json", `[
	 {"id":"g1","question":"Has the owner heard Osho's Ashtavakra Mahageeta?","expect_all":["yes"],"source":"trace-x"},
	 {"id":"g2","question":"","expect_all":["x"]},
	 {"id":"g3","question":"q","expect_all":[]}]`)
	r, _ := Check(root)
	if f := problems(r, "fail"); len(f) != 2 {
		t.Fatalf("want g2 and g3 failing, got %v", f)
	}
	if len(r.Golden) != 3 {
		t.Fatalf("golden: %+v", r.Golden)
	}
}

func TestGoldenMayForbidPhrases(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/golden.json", `[
	 {"id":"g1","question":"Suggest a podcast","expect_none":["Ashtavakra"]},
	 {"id":"g2","question":"q"}]`)
	r, _ := Check(root)
	f := problems(r, "fail")
	if len(f) != 1 || !strings.Contains(f[0], "g2") { // g1 forbids something, so it can fail; g2 can never fail
		t.Fatalf("want only g2 failing, got %v", f)
	}
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	root := memory(t)
	write(t, root, ".tests/probes.json", `[{"id":"p1","fact":"`+idA+`","kind":"direct","question":"q","expected":"e","answr":"typo"}]`)
	if _, err := Check(root); err == nil {
		t.Fatal("a misspelt field must be an error, not silently ignored")
	}
}

// Probes repeat facts, so they are personal data and must never be tracked by the public repo.
func TestNoTestDataIsTrackedInThisRepo(t *testing.T) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skip("not a git checkout")
	}
	for _, f := range strings.Split(string(out), "\n") {
		if strings.Contains(f, ".tests/") || strings.HasSuffix(f, "golden.json") || strings.HasSuffix(f, "probes.json") {
			t.Errorf("tracked file looks like memory test data: %s", f)
		}
	}
}
