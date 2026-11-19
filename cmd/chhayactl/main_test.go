package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/satishthakur/chhaya/internal/jobreport"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/nightly"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
		"dreamer:     memory.list, memory.get, memory.propose_update only; 5 proposals a day",
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
	if err := runTrace([]string{"export", "-home", home, "-out", t.TempDir(), "-gap", "2h"}, &out, d); err != nil {
		t.Errorf("-gap takes a duration: %v", err)
	}
	if err := runTrace([]string{"export", "-gap", "later"}, &out, d); err == nil {
		t.Error("a bad -gap must be an error")
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

// writeTrace puts a trace file where the store looks for it.
func writeTrace(t *testing.T, dir, day, id string, body string) {
	t.Helper()
	p := filepath.Join(dir, "days", day, "trace-"+id+".json")
	must(t, os.MkdirAll(filepath.Dir(p), 0o700))
	must(t, os.WriteFile(p, []byte(body), 0o600))
}

const cleanTrace = `{"schema":2,"id":"%s","day":"%s","source":"telegram","started":"2026-11-11T10:00:00Z",
 "messages":[{"id":1,"role":"user","content":"hi"},{"id":2,"role":"assistant","content":"hello"}],
 "counters":{"memory_calls_before_first_question":-1}}`

// the owner wrote, and the only thing after it is a tool call: no text reply
const unansweredTrace = `{"schema":2,"id":"%s","day":"%s","source":"telegram","started":"2026-11-11T10:00:00Z",
 "messages":[{"id":1,"role":"user","content":"hi"},{"id":2,"role":"assistant","calls":[{"id":"c","name":"x"}]},{"id":3,"role":"tool","tool_call_id":"c","content":"ok"}],
 "counters":{"memory_calls_before_first_question":-1}}`

func tf(body, id, day string) string {
	s := strings.Replace(body, "%s", id, 1)
	return strings.Replace(s, "%s", day, 1)
}

func TestTraceCheckReportsFindingsAndExitsNonZeroOnAFailure(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "2026-11-11", "good", tf(cleanTrace, "good", "2026-11-11"))
	writeTrace(t, dir, "2026-11-11", "bad", tf(unansweredTrace, "bad", "2026-11-11"))
	var out bytes.Buffer
	err := runTrace([]string{"check", "-dir", dir}, &out, traceDeps{now: time.Now})
	if err == nil || !strings.Contains(err.Error(), "1 failure") {
		t.Fatalf("a fail finding must be an error, got %v", err)
	}
	for _, want := range []string{"checked 2 trace(s): 1 clean, 1 with findings", "reply.unanswered_human_message", "msg 1", "bad", "1 failure(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "  good  ") {
		t.Errorf("a clean trace must not be listed:\n%s", out.String())
	}
}

func TestTraceCheckCleanRunSucceedsAndFiltersByDay(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "2026-11-10", "old", tf(unansweredTrace, "old", "2026-11-10"))
	writeTrace(t, dir, "2026-11-11", "new", tf(cleanTrace, "new", "2026-11-11"))
	var out bytes.Buffer
	if err := runTrace([]string{"check", "-dir", dir, "-day", "2026-11-11"}, &out, traceDeps{now: time.Now}); err != nil {
		t.Fatalf("the failing trace is on another day: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "checked 1 trace(s): 1 clean") {
		t.Errorf("output = %s", out.String())
	}
	if err := runTrace([]string{"check", "-dir", dir, "-day", "yesterday"}, &out, traceDeps{now: time.Now}); err == nil {
		t.Error("a bad -day must be an error")
	}
}

func TestTraceCheckNeverPassesATraceItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "2026-11-11", "broken", "{not json")
	var out bytes.Buffer
	err := runTrace([]string{"check", "-dir", dir}, &out, traceDeps{now: time.Now})
	if err == nil {
		t.Fatalf("an unreadable trace is not a pass:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "could NOT be checked") || !strings.Contains(out.String(), "broken") {
		t.Errorf("output = %s", out.String())
	}
}

func TestTraceCheckWithS3ReadsTheBucketNotAStaleLocalCopy(t *testing.T) {
	dir := t.TempDir()
	// the local copy is the failing one; the bucket's newer copy is clean
	writeTrace(t, dir, "2026-11-11", "s1", tf(unansweredTrace, "s1", "2026-11-11"))
	d := traceDeps{now: time.Now, run: func(name string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "s3" && args[1] == "ls":
			return []byte("2026-11-11 10:00:00 100 evals/days/2026-11-11/trace-s1.json\n"), nil
		case len(args) >= 2 && args[0] == "s3" && args[1] == "cp":
			return []byte(tf(cleanTrace, "s1", "2026-11-11")), nil
		}
		return nil, errors.New("unexpected aws call")
	}}
	var out bytes.Buffer
	if err := runTrace([]string{"check", "-dir", dir, "-s3", "s3://b/evals"}, &out, d); err != nil {
		t.Fatalf("must have checked the bucket's clean copy: %v\n%s", err, out.String())
	}
}

func TestTraceCheckJSONIsParseable(t *testing.T) {
	dir := t.TempDir()
	writeTrace(t, dir, "2026-11-11", "bad", tf(unansweredTrace, "bad", "2026-11-11"))
	var out bytes.Buffer
	_ = runTrace([]string{"check", "-dir", dir, "-json"}, &out, traceDeps{now: time.Now})
	var rep struct {
		Summary  []struct{ Check string }
		Traces   []struct{ ID string }
		Problems []string
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if len(rep.Traces) != 1 || len(rep.Summary) == 0 || rep.Problems == nil {
		t.Errorf("report = %+v", rep)
	}
}

func TestMemtestCheckReportsAndFailsOnABrokenFact(t *testing.T) {
	root := t.TempDir()
	good := "# Interests\n\n- Loves books. <!-- proposal 20261106-3f609264ada59fd2, approved 2026-11-06 -->\n"
	if err := os.WriteFile(filepath.Join(root, "interests.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runMemtest([]string{"check", "-memory", root}, &out); err != nil {
		t.Fatalf("a clean memory must pass: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 fact(s)") || !strings.Contains(out.String(), "warn") {
		t.Fatalf("want a count and a no-probe warning:\n%s", out.String())
	}
	if err := os.WriteFile(filepath.Join(root, "location.md"), []byte("# Location\n\n- Lives in Pune.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runMemtest([]string{"check", "-memory", root}, &out); err == nil || !strings.Contains(out.String(), "fail") {
		t.Fatalf("a line with no id must fail:\n%s", out.String())
	}
	if err := runMemtest([]string{"nonsense"}, &out); err == nil {
		t.Fatal("an unknown subcommand must be an error")
	}
}

type fakeMemModel struct{ replies []memrun.Message }

func (f *fakeMemModel) Chat(_ context.Context, _ []memrun.Message, _ []memrun.ToolDef) (memrun.Message, memrun.Usage, error) {
	r := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return r, memrun.Usage{In: 50, Out: 5}, nil
}

func TestMemtestRunFailsWhenAFactIsNeverRetrieved(t *testing.T) {
	root := t.TempDir()
	id := "20261106-3f609264ada59fd2"
	os.WriteFile(filepath.Join(root, "interests.md"), []byte("# Interests\n\n- Loves books. <!-- proposal "+id+", approved 2026-11-06 -->\n"), 0o644)
	os.MkdirAll(filepath.Join(root, ".tests"), 0o755)
	os.WriteFile(filepath.Join(root, ".tests", "probes.json"), []byte(`[{"id":"p1","fact":"`+id+`","kind":"direct","question":"What does the owner love?","expected":"books"}]`), 0o644)
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("PROXY_API_KEY=sk-test-key\nCHHAYA_HERMES_API_BASE=http://example.invalid/v1\n"), 0o600)

	var out bytes.Buffer
	newMemModel = func(base, key, model string) memrun.Model {
		if key != "sk-test-key" || base != "http://example.invalid/v1" {
			t.Errorf("model built with key=%q base=%q", key, base)
		}
		return &fakeMemModel{replies: []memrun.Message{{Role: "assistant", Content: "Probably books."}}} // never reads memory
	}
	err := runMemtest([]string{"run", "-memory", root, "-env-file", envf, "-judge=false"}, &out)
	if err == nil || !strings.Contains(out.String(), "MISS") {
		t.Fatalf("a guess is not a retrieval; want a failure naming MISS:\n%s\n%v", out.String(), err)
	}
	if strings.Contains(out.String(), "sk-test-key") {
		t.Fatal("the key was printed")
	}

	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{
			{Role: "assistant", ToolCalls: []memrun.ToolCall{{ID: "c", Name: "memory_get", Args: `{"path":"interests"}`}}},
			{Role: "assistant", Content: "Books."}}}
	}
	out.Reset()
	if err := runMemtest([]string{"run", "-memory", root, "-env-file", envf, "-judge=false"}, &out); err != nil {
		t.Fatalf("reading the document is a retrieval: %v\n%s", err, out.String())
	}
}

func TestMemtestRunNeedsAKey(t *testing.T) {
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("CHHAYA_HERMES_API_BASE=http://x/v1\n"), 0o600)
	t.Setenv("PROXY_API_KEY", "")
	var out bytes.Buffer
	if err := runMemtest([]string{"run", "-memory", t.TempDir(), "-env-file", envf}, &out); err == nil || !strings.Contains(err.Error(), "PROXY_API_KEY") {
		t.Fatalf("want an error naming PROXY_API_KEY, got %v", err)
	}
}

type nightSender struct{ got []string }

func (r *nightSender) Send(_ context.Context, s string) error { r.got = append(r.got, s); return nil }

func TestMemtestRunWritesJSONEvenWhenTheGateFails(t *testing.T) {
	root := t.TempDir()
	id := "20261106-3f609264ada59fd2"
	os.WriteFile(filepath.Join(root, "interests.md"), []byte("# Interests\n\n- Loves books. <!-- proposal "+id+", approved 2026-11-06 -->\n"), 0o644)
	os.MkdirAll(filepath.Join(root, ".tests"), 0o755)
	os.WriteFile(filepath.Join(root, ".tests", "probes.json"), []byte(`[{"id":"p1","fact":"`+id+`","kind":"direct","question":"What?","expected":"books"}]`), 0o644)
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("PROXY_API_KEY=k\nCHHAYA_HERMES_API_BASE=http://x/v1\n"), 0o600)
	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{{Role: "assistant", Content: "guess"}}}
	}
	jp := filepath.Join(t.TempDir(), "memrun.json")
	var out bytes.Buffer
	if err := runMemtest([]string{"run", "-memory", root, "-env-file", envf, "-judge=false", "-json", jp}, &out); err == nil {
		t.Fatal("the gate should have failed")
	}
	rf, err := nightly.ReadRunFile(jp)
	if err != nil || rf.Result.Gate() != 1 {
		t.Fatalf("a failing night must still leave its result for the report: %+v %v", rf, err)
	}
}

func TestReportNightlyPrintsSendsAndNeverLeaksText(t *testing.T) {
	evals := t.TempDir()
	day := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	tr := `{"schema":2,"id":"t1","day":"2026-11-18","source":"telegram","started":"2026-11-18T01:00:00Z","last_message":"2026-11-18T01:05:00Z","messages":[` +
		`{"id":1,"role":"user","time":"2026-11-18T01:00:00Z","content":"suggest a podcast about SECRETWORD"},` +
		`{"id":2,"role":"assistant","time":"2026-11-18T01:00:05Z","content":"Try Sarvapriyananda."}]}`
	os.MkdirAll(filepath.Join(evals, "days", "2026-11-18"), 0o755)
	os.WriteFile(filepath.Join(evals, "days", "2026-11-18", "trace-t1.json"), []byte(tr), 0o644)
	mem := t.TempDir()
	os.WriteFile(filepath.Join(mem, "i.md"), []byte("# I\n\n- Loves books. <!-- proposal 20261106-3f609264ada59fd2, approved 2026-11-06 -->\n"), 0o644)
	rj := filepath.Join(t.TempDir(), "memrun.json")
	nightly.WriteRunFile(rj, nightly.RunFile{RanAt: day.Add(-5 * time.Hour), Model: "m"})
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("CHHAYA_APPROVER_BOT_TOKEN=tok-secret\nCHHAYA_APPROVER_ALLOWED_USERS=42\n"), 0o600)

	rec := &nightSender{}
	d := reportDeps{now: func() time.Time { return day },
		run:    func(string, ...string) ([]byte, error) { return nil, errors.New("no aws here") },
		sender: func(tok string, chat int64) nightly.Sender { return rec }}
	var out bytes.Buffer
	err := runReport([]string{"report-ignored"}, &out, d)
	if err == nil {
		t.Fatal("a bad subcommand must be an error")
	}
	out.Reset()
	if err := runReport([]string{"nightly", "-dir", evals, "-memory", mem, "-run-json", rj, "-env", envf, "-send"}, &out, d); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(rec.got) != 1 || !strings.Contains(rec.got[0], "traces: 1 checked") {
		t.Fatalf("sent %v", rec.got)
	}
	if strings.Contains(rec.got[0]+out.String(), "SECRETWORD") || strings.Contains(out.String(), "tok-secret") {
		t.Fatal("message text or the bot token leaked")
	}
}

func TestDreamEvalRunsTheShippedCasesAndFailsOnAMiss(t *testing.T) {
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("PROXY_API_KEY=k\nCHHAYA_HERMES_API_BASE=http://x/v1\n"), 0o600)
	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{{Role: "assistant", Content: `{"proposals":[]}`}}}
	}
	var out bytes.Buffer
	err := runDream([]string{"eval", "-cases", "../../internal/dream/cases", "-env-file", envf}, &out)
	if err == nil || !regexp.MustCompile(`FAIL +recall-heard`).MatchString(out.String()) || !regexp.MustCompile(`pass +silence-greeting`).MatchString(out.String()) {
		t.Fatalf("a model that proposes nothing must fail the recall cases and pass the silence ones:\n%s\n%v", out.String(), err)
	}
	if err := runDream([]string{"nonsense"}, &out); err == nil {
		t.Fatal("an unknown subcommand must be an error")
	}
}

type fakeDreamGW struct{ proposed []string }

func (f *fakeDreamGW) List(context.Context) ([]string, error) { return []string{"interests"}, nil }
func (f *fakeDreamGW) Get(context.Context, string) (string, error) {
	return "# I\n\n- Loves books. <!-- proposal 20261106-3f609264ada59fd2, approved 2026-11-06 -->\n", nil
}
func (f *fakeDreamGW) Propose(_ context.Context, p, e, r string) error {
	f.proposed = append(f.proposed, p+"|"+e+"|"+r)
	return nil
}
func (f *fakeDreamGW) Close() error { return nil }

func TestDreamRunDryRunThenProposeOnAnOldTrace(t *testing.T) {
	evals := t.TempDir()
	os.MkdirAll(filepath.Join(evals, "days", "2026-11-16"), 0o755)
	tr := `{"schema":2,"id":"old1","day":"2026-11-16","source":"telegram","started":"2026-11-16T04:00:00Z","last_message":"2026-11-16T04:05:00Z","messages":[` +
		`{"id":7,"role":"user","time":"2026-11-16T04:00:00Z","content":"I already heard the Foo series"},` +
		`{"id":8,"role":"assistant","time":"2026-11-16T04:00:05Z","content":"Noted."}]}`
	os.WriteFile(filepath.Join(evals, "days", "2026-11-16", "trace-old1.json"), []byte(tr), 0o644)
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("PROXY_API_KEY=k\nCHHAYA_HERMES_API_BASE=http://x/v1\n"), 0o600)
	call := `{"path":"interests","entry":"Has already heard the Foo series.","rationale":"said so","source":[7]}`
	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{
			{Role: "assistant", ToolCalls: []memrun.ToolCall{{ID: "1", Name: "propose_fact", Args: call}}},
			{Role: "assistant", Content: "done"},
		}}
	}
	gw := &fakeDreamGW{}
	d := dreamRunDeps{now: func() time.Time { return time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC) },
		run:     func(string, ...string) ([]byte, error) { return nil, errors.New("no aws") },
		connect: func(context.Context, string, string) (dreamGateway, error) { return gw, nil }}
	ledger := filepath.Join(t.TempDir(), "ledger.json")
	base := []string{"-dir", evals, "-day", "2026-11-16", "-env-file", envf, "-ledger", ledger}

	var out bytes.Buffer
	if err := runDreamRun(base, &out, d); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(gw.proposed) != 0 || !strings.Contains(out.String(), "would propose (dry run)") {
		t.Fatalf("a dry run must send nothing:\n%s", out.String())
	}

	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{
			{Role: "assistant", ToolCalls: []memrun.ToolCall{{ID: "1", Name: "propose_fact", Args: call}}},
			{Role: "assistant", Content: "done"},
		}}
	}
	out.Reset()
	if err := runDreamRun(append(base, "-propose"), &out, d); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(gw.proposed) != 1 || !strings.Contains(gw.proposed[0], "trace old1, msg 7") {
		t.Fatalf("%v\n%s", gw.proposed, out.String())
	}
	// a dry run with -html leaves a review page that quotes the owner
	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{
			{Role: "assistant", ToolCalls: []memrun.ToolCall{{ID: "1", Name: "propose_fact", Args: call}}},
			{Role: "assistant", Content: "done"},
		}}
	}
	page := filepath.Join(t.TempDir(), "review.html")
	out.Reset()
	if err := runDreamRun(append(append([]string{}, base...), "-html", page), &out, d); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(page); !strings.Contains(string(b), "Would propose (1)") || !strings.Contains(string(b), "I already heard the Foo series") {
		t.Fatalf("the review page lacks the proposal or the owner's words:\n%s", b)
	}
	// the conversation is now in the ledger: a second real run reads nothing
	out.Reset()
	if err := runDreamRun(append(base, "-propose"), &out, d); err != nil || !strings.Contains(out.String(), "read 0, skipped 1") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if err := runDreamRun([]string{"-trace", "nope", "-dir", evals, "-env-file", envf}, &out, d); err == nil {
		t.Fatal("an unknown trace id must be an error")
	}
}

func TestDreamRunSendsASummaryWithNoTextAndALink(t *testing.T) {
	evals := t.TempDir()
	os.MkdirAll(filepath.Join(evals, "days", "2026-11-16"), 0o755)
	tr := `{"schema":2,"id":"old1","day":"2026-11-16","source":"telegram","started":"2026-11-16T04:00:00Z","last_message":"2026-11-16T04:05:00Z","messages":[` +
		`{"id":7,"role":"user","time":"2026-11-16T04:00:00Z","content":"I already heard the SECRETSERIES"},` +
		`{"id":8,"role":"assistant","time":"2026-11-16T04:00:05Z","content":"Noted."}]}`
	os.WriteFile(filepath.Join(evals, "days", "2026-11-16", "trace-old1.json"), []byte(tr), 0o644)
	envf := filepath.Join(t.TempDir(), "env")
	os.WriteFile(envf, []byte("PROXY_API_KEY=k\nCHHAYA_HERMES_API_BASE=http://x/v1\n"), 0o600)
	gwenv := filepath.Join(t.TempDir(), "genv")
	os.WriteFile(gwenv, []byte("CHHAYA_APPROVER_BOT_TOKEN=tok-secret\nCHHAYA_APPROVER_ALLOWED_USERS=42\n"), 0o600)
	call := `{"path":"interests","entry":"Has already heard the SECRETSERIES.","rationale":"said so","source":[7]}`
	newMemModel = func(string, string, string) memrun.Model {
		return &fakeMemModel{replies: []memrun.Message{
			{Role: "assistant", ToolCalls: []memrun.ToolCall{{ID: "1", Name: "propose_fact", Args: call}}},
			{Role: "assistant", Content: "done"},
		}}
	}
	rec := &nightSender{}
	var aws []string
	d := dreamRunDeps{now: func() time.Time { return time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC) },
		run: func(name string, args ...string) ([]byte, error) {
			aws = append(aws, strings.Join(args, " "))
			if len(args) > 1 && args[1] == "presign" {
				return []byte("https://signed/x\n"), nil
			}
			return []byte("{}"), nil // the trace store lists nothing in the bucket; the local dir is used below
		},
		connect: func(context.Context, string, string) (dreamGateway, error) { return &fakeDreamGW{}, nil },
		sender:  func(string, int64) jobreport.Sender { return rec }}
	var out bytes.Buffer
	// -s3 is needed for the link, but traces are read from -dir here: give both
	err := runDreamRun([]string{"-s3", "s3://b/evals", "-dir", evals, "-day", "2026-11-16", "-env-file", envf, "-env", gwenv, "-send", "-ledger", filepath.Join(t.TempDir(), "l.json")}, &out, d)
	if len(rec.got) != 1 {
		t.Fatalf("one summary must be sent (err %v):\n%s", err, out.String())
	}
	m := rec.got[0]
	if !strings.Contains(m, "https://signed/x") || !strings.Contains(m, "dry run") || strings.Contains(m, "SECRETSERIES") || strings.Contains(m+out.String(), "tok-secret") {
		t.Fatalf("summary: %s", m)
	}
	// a run that cannot reach the gateway still reports
	rec.got = nil
	d.connect = func(context.Context, string, string) (dreamGateway, error) {
		return nil, errors.New("connection refused")
	}
	err = runDreamRun([]string{"-s3", "s3://b/evals", "-dir", evals, "-day", "2026-11-16", "-env-file", envf, "-env", gwenv, "-send"}, &out, d)
	if err == nil || len(rec.got) != 1 || !strings.Contains(rec.got[0], "FAILED") || !strings.Contains(rec.got[0], "connection refused") {
		t.Fatalf("a run that could not start must say so: %v %v", err, rec.got)
	}
	if err := runDreamRun([]string{"-send", "-dir", evals, "-env-file", envf, "-env", gwenv}, &out, d); err == nil {
		t.Fatal("-send without -s3 has nowhere to put the page and must be refused")
	}
}
