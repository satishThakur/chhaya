package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmpLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "audit.jsonl")
}

func rec(tool string) Record {
	return Record{
		Actor: "hermes", Tool: tool, Kind: "write", Tier: 0,
		Args:     mustRaw(map[string]any{"path": "taste/food"}),
		Decision: "deny", Rule: "killswitch.active", Reason: "kill switch active",
		Result: Result{Status: "refused"}, DurationMS: 3,
	}
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestAppendBuildsAChainThatVerifies(t *testing.T) {
	p := tmpLog(t)
	w, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ping", "memory.get", "memory.propose_update"} {
		if _, err := w.Append(rec(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := Verify(p)
	if err != nil {
		t.Fatalf("a log we just wrote must verify: %v", err)
	}
	if n != 3 {
		t.Errorf("verified %d records, want 3", n)
	}
}

func TestGenesisRecordHasNoPrev(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	got, err := w.Append(rec("ping"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	if got.Seq != 1 {
		t.Errorf("seq = %d, want 1", got.Seq)
	}
	if got.Prev != nil {
		t.Errorf("genesis prev = %v, want null", *got.Prev)
	}
	if !strings.HasPrefix(got.Hash, "sha256:") {
		t.Errorf("hash = %q, want a sha256: prefix", got.Hash)
	}
}

func TestMutatingAPastRecordFailsVerification(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	for i := 0; i < 3; i++ {
		w.Append(rec("ping"))
	}
	w.Close()

	lines := readLines(t, p)
	var r Record
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil {
		t.Fatal(err)
	}
	r.Decision = "allow" // the edit an attacker would actually want
	b, _ := json.Marshal(r)
	lines[1] = string(b)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	if _, err := Verify(p); err == nil {
		t.Fatal("flipping a past decision must break the chain")
	}
}

func TestTruncationFailsVerification(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	for i := 0; i < 4; i++ {
		w.Append(rec("ping"))
	}
	w.Close()

	// Drop a record from the middle — the case a naive "recompute the tail"
	// verifier would miss.
	lines := readLines(t, p)
	kept := append(append([]string{}, lines[:2]...), lines[3:]...)
	os.WriteFile(p, []byte(strings.Join(kept, "\n")+"\n"), 0o600)

	if _, err := Verify(p); err == nil {
		t.Fatal("a removed record must break the chain")
	}
}

func TestOpenRefusesABrokenChain(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	w.Append(rec("ping"))
	w.Append(rec("ping"))
	w.Close()

	lines := readLines(t, p)
	lines[0] = strings.Replace(lines[0], `"tool":"ping"`, `"tool":"pong"`, 1)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	if _, err := Open(p); err == nil {
		t.Fatal("a broken chain is a refusal to start, not a warning")
	}
}

func TestReopenContinuesTheChain(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	first, _ := w.Append(rec("ping"))
	w.Close()

	w2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w2.Append(rec("ping"))
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()

	if second.Seq != 2 {
		t.Errorf("seq after reopen = %d, want 2", second.Seq)
	}
	if second.Prev == nil || *second.Prev != first.Hash {
		t.Error("a restart must not start a new chain")
	}
	if _, err := Verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestRedactionHappensBeforeHashing(t *testing.T) {
	const secret = "8979753688:AAH-SUPER-SECRET-TOKEN"
	p := tmpLog(t)
	w, _ := Open(p)

	args := Redact(map[string]any{
		"path":  "taste/food",
		"diff":  secret,
		"inner": map[string]any{"authorization": "Bearer " + secret},
	}, []string{"diff"})

	r := rec("memory.propose_update")
	r.Args = mustRaw(args)
	got, err := w.Append(r)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the secret reached the log, and therefore the chain")
	}
	if !strings.Contains(string(got.Args), "<redacted:") {
		t.Errorf("args = %s, want a redaction marker", got.Args)
	}
	if !strings.Contains(string(got.Args), "taste/food") {
		t.Error("redaction should scrub the named path, not everything")
	}
}

func TestRedactScrubsSecretishKeysWithoutBeingAsked(t *testing.T) {
	out := Redact(map[string]any{
		"bot_token":     "abc123",
		"nested":        map[string]any{"api_key": "def456", "path": "ok"},
		"harmless":      "visible",
	}, nil)

	b, _ := json.Marshal(out)
	s := string(b)
	for _, leaked := range []string{"abc123", "def456"} {
		if strings.Contains(s, leaked) {
			t.Errorf("%q survived default redaction: %s", leaked, s)
		}
	}
	if !strings.Contains(s, "visible") || !strings.Contains(s, "ok") {
		t.Errorf("default redaction was too greedy: %s", s)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}
