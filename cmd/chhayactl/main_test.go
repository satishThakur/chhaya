package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/state"
)

func TestAuditVerifyPassesOnAGoodLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	w, err := audit.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(audit.Record{Tool: "ping", Kind: "read", Decision: "allow", Args: json.RawMessage(`{}`)})
	w.Close()

	var out bytes.Buffer
	if err := runVerify(p, &out); err != nil {
		t.Fatalf("verify failed on a log we just wrote: %v", err)
	}
	if !strings.Contains(out.String(), "1") {
		t.Errorf("output should state how many records were checked: %q", out.String())
	}
}

func TestAuditVerifyFailsOnATamperedLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	w, _ := audit.Open(p)
	w.Append(audit.Record{Tool: "memory.propose_update", Kind: "write", Decision: "deny", Args: json.RawMessage(`{}`)})
	w.Close()

	b, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(b, []byte(`"decision":"deny"`), []byte(`"decision":"allow"`), 1), 0o600)

	if err := runVerify(p, &bytes.Buffer{}); err == nil {
		t.Fatal("verify passed on a log whose decision was flipped")
	}
}

func TestStopSetsTheKillSwitchWithoutTheDaemon(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	if err := runStop(p, true); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Snapshot(time.Now()).KillSwitch {
		t.Fatal("chhayactl stop must work when the daemon is not running; that is the backstop")
	}
}
