package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
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

type fakeBot struct {
	id   int64
	name string
	err  error
}

func (f fakeBot) GetMe(context.Context) (int64, string, error) { return f.id, f.name, f.err }

func botEnvFiles(t *testing.T, hermesEnv, gatewayEnv string) (string, string) {
	t.Helper()
	d := t.TempDir()
	h, g := filepath.Join(d, ".env.hermes"), filepath.Join(d, ".env.gateway")
	os.WriteFile(h, []byte("CHHAYA_ENV="+hermesEnv+"\nTELEGRAM_BOT_TOKEN=111:hermes-secret\n"), 0o600)
	os.WriteFile(g, []byte("CHHAYA_ENV="+gatewayEnv+"\nCHHAYA_APPROVER_BOT_TOKEN=222:approver-secret\n"), 0o600)
	return h, g
}

func TestBotCheckPassesForTheMatchingPairAndNeverPrintsATokenOrReadsTheWrongOne(t *testing.T) {
	h, g := botEnvFiles(t, "dev", "dev")
	var gotTokens []string
	mk := func(token string) botGetter {
		gotTokens = append(gotTokens, token)
		if token == "111:hermes-secret" {
			return fakeBot{1, "chhaya_dev_bot", nil}
		}
		return fakeBot{2, "chhaya_dev_approver_bot", nil}
	}
	var out bytes.Buffer
	if err := runBotCheck(h, g, mk, &out); err != nil {
		t.Fatalf("matching dev pair must pass: %v", err)
	}
	if strings.Contains(out.String(), "secret") {
		t.Errorf("output must never contain a token: %q", out.String())
	}
	if len(gotTokens) != 2 {
		t.Errorf("expected one lookup per token, got %v", gotTokens)
	}
}

func TestBotCheckRefusesProdBotsInADevEnvironment(t *testing.T) {
	h, g := botEnvFiles(t, "dev", "dev")
	mk := func(token string) botGetter {
		if token == "111:hermes-secret" {
			return fakeBot{1, "chhaya_bot", nil}
		}
		return fakeBot{2, "chhaya_approver_bot", nil}
	}
	var out bytes.Buffer
	err := runBotCheck(h, g, mk, &out)
	if err == nil {
		t.Fatal("prod bots in a dev environment must be refused")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error must never contain a token: %v", err)
	}
}

func TestBotCheckRefusesWhenATokenIsMissing(t *testing.T) {
	h, g := botEnvFiles(t, "dev", "dev")
	os.WriteFile(g, []byte("CHHAYA_ENV=dev\nCHHAYA_APPROVER_BOT_TOKEN=\n"), 0o600)
	mk := func(string) botGetter { return fakeBot{1, "chhaya_dev_bot", nil} }
	if err := runBotCheck(h, g, mk, &bytes.Buffer{}); err == nil {
		t.Fatal("an empty token must be refused, not skipped")
	}
}

func TestBotCheckRefusesWhenAnEnvFileIsMissing(t *testing.T) {
	h, g := botEnvFiles(t, "dev", "dev")
	os.Remove(g)
	mk := func(string) botGetter { return fakeBot{1, "chhaya_dev_bot", nil} }
	if err := runBotCheck(h, g, mk, &bytes.Buffer{}); err == nil {
		t.Fatal("a missing env file must be refused")
	}
}

func testBackupDeps(now time.Time) backupDeps {
	return backupDeps{
		now:   func() time.Time { return now },
		check: func(string) error { return nil },
		run:   func(string, ...string) error { return nil },
	}
}

func TestBackupStatusFailsWithNoBackupAndPassesAfterPush(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".chhaya")
	for _, d := range []string{"hermes", "gateway"} {
		os.MkdirAll(filepath.Join(home, d), 0o700)
	}
	os.WriteFile(filepath.Join(home, "hermes", "a"), []byte("x"), 0o600)
	now := time.Date(2026, 11, 10, 20, 30, 0, 0, time.UTC)
	d := testBackupDeps(now)

	if err := runBackup([]string{"status", "-home", home}, &bytes.Buffer{}, d); err == nil {
		t.Fatal("status must fail before any backup")
	}
	var out bytes.Buffer
	if err := runBackup([]string{"create", "-home", home}, &out, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	archive := strings.TrimSpace(out.String())
	if err := runBackup([]string{"verify", archive}, &bytes.Buffer{}, d); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := runBackup([]string{"push", "-home", home, "-s3", "s3://b/backups", archive}, &bytes.Buffer{}, d); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := runBackup([]string{"status", "-home", home}, &bytes.Buffer{}, d); err != nil {
		t.Fatalf("status after push: %v", err)
	}
	if err := runBackup([]string{"status", "-home", home}, &bytes.Buffer{}, testBackupDeps(now.Add(40*time.Hour))); err == nil {
		t.Fatal("status must fail once the backup is older than 36h")
	}
}

func TestBackupRejectsUnknownCommandsAndMissingArguments(t *testing.T) {
	d := testBackupDeps(time.Now())
	for _, args := range [][]string{{}, {"nope"}, {"verify"}, {"push", "x"}, {"pull", "s3://a/b"}} {
		if err := runBackup(args, &bytes.Buffer{}, d); err == nil {
			t.Errorf("backup %v must be an error", args)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTraceExportReportsAndPassesItsFlags(t *testing.T) {
	var gotArgs [][]string
	d := traceDeps{
		now:   func() time.Time { return time.Unix(1794203400, 0) },
		query: func(db, sql string) ([]byte, error) { return nil, nil },
		run: func(name string, args ...string) ([]byte, error) {
			gotArgs = append(gotArgs, args)
			return nil, nil
		},
	}
	home := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, "gateway"), 0o700))
	must(t, os.WriteFile(filepath.Join(home, "gateway", "audit.jsonl"), nil, 0o600))
	var out bytes.Buffer
	if err := runTrace([]string{"export", "-home", home, "-out", t.TempDir(), "-since", "3d"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "exported 0 trace(s)") {
		t.Errorf("output = %q", out.String())
	}
	if err := runTrace([]string{"export", "-since", "soon"}, &out, d); err == nil {
		t.Error("a bad -since must be an error")
	}
	if err := runTrace([]string{"export", "extra"}, &out, d); err == nil {
		t.Error("stray arguments must be an error")
	}
	if err := runTrace([]string{"frobnicate"}, &out, d); err == nil {
		t.Error("unknown subcommand must be an error")
	}
}

func TestTraceServeBindsOnlyToLoopback(t *testing.T) {
	var bound string
	d := traceDeps{
		now: time.Now,
		serve: func(ln net.Listener, h http.Handler) error {
			bound = ln.Addr().String()
			return nil
		},
	}
	var out bytes.Buffer
	if err := runTrace([]string{"serve", "-dir", t.TempDir(), "-port", "0"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bound, "127.0.0.1:") {
		t.Fatalf("listening on %q; the page shows private conversations and must never leave loopback", bound)
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:") {
		t.Errorf("output = %q", out.String())
	}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"2d": 48 * time.Hour, "36h": 36 * time.Hour, "0.5d": 12 * time.Hour} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "-1d", "x", "-3h"} {
		if _, err := parseSince(bad); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}
