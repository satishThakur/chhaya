package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
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
	output := out.String()
	if !strings.Contains(output, "ok: 1 records") {
		t.Errorf("output should contain record count: %q", output)
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

	var out bytes.Buffer
	if err := runVerify(p, &out); err == nil {
		t.Fatal("verify passed on a log whose decision was flipped")
	}
	if strings.Contains(out.String(), "intact") {
		t.Errorf("output should not claim chain is intact after tampering: %q", out.String())
	}
}

func TestAuditVerifyErrorsOnMissingLog(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "nonexistent.jsonl")

	var out bytes.Buffer
	verifyErr := runVerify(missingPath, &out)
	if verifyErr == nil {
		t.Fatal("verify should error on a missing log")
	}
	if !errors.Is(verifyErr, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got: %v", verifyErr)
	}
	if !strings.Contains(out.String(), "no audit log") {
		t.Errorf("output should report missing log distinctly: %q", out.String())
	}
}

func TestStopSetsTheKillSwitchWithoutTheDaemon(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	var out bytes.Buffer
	if err := runStop(p, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kill switch ON") {
		t.Errorf("output should indicate kill switch is on: %q", out.String())
	}
	st, err := state.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Snapshot(time.Now()).KillSwitch {
		t.Fatal("chhayactl stop must work when the daemon is not running; that is the backstop")
	}
}

func TestRunExplainPrintsConfiguration(t *testing.T) {
	// Use the actual config file from the project
	policyPath := "../../config/policy.yaml"
	_, err := os.Stat(policyPath)
	if err != nil {
		t.Skipf("config file not found at %s", policyPath)
	}

	var out bytes.Buffer
	if err := runExplain(policyPath, &out); err != nil {
		t.Fatalf("runExplain failed: %v", err)
	}

	output := out.String()
	expectedStrings := []string{
		"policy hash:",
		"rate limit:",
		"approvals:",
		"branches:",
		"cap food:",
		"per order",
		"per day",
		"Tiers are decided in internal/policy",
	}
	for _, expected := range expectedStrings {
		if !strings.Contains(output, expected) {
			t.Errorf("output missing expected string %q: %q", expected, output)
		}
	}
}
