package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureHome builds a ~/.chhaya with a real SQLite state.db (made by the sqlite3 CLI, the
// same program the reader uses) and an audit.jsonl. Epoch 1794203280 is 2026-11-09 05:48:00 UTC.
func fixtureHome(t *testing.T, schemaSQL, dataSQL, audit string) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not installed")
	}
	home := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, "hermes"), 0o700))
	must(t, os.MkdirAll(filepath.Join(home, "gateway"), 0o700))
	cmd := exec.Command("sqlite3", filepath.Join(home, "hermes", "state.db"))
	cmd.Stdin = strings.NewReader(schemaSQL + dataSQL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fixture db: %v: %s", err, out)
	}
	must(t, os.WriteFile(filepath.Join(home, "gateway", "audit.jsonl"), []byte(audit), 0o600))
	return home
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const schema = `
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, chat_id TEXT, model TEXT, title TEXT,
  parent_session_id TEXT, started_at REAL NOT NULL, input_tokens INTEGER, output_tokens INTEGER,
  estimated_cost_usd REAL, actual_cost_usd REAL, system_prompt_hash TEXT);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
  content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, finish_reason TEXT, timestamp REAL NOT NULL,
  active INTEGER DEFAULT 1, compacted INTEGER DEFAULT 0);
`

const data = `
INSERT INTO sessions VALUES ('s_recent','telegram','42','claude-sonnet-5','Exercise',NULL,1794203279,100,20,0.0,0.5,'hash-abc');
INSERT INTO sessions VALUES ('s_old','telegram','42',NULL,NULL,NULL,1700000000,0,0,NULL,NULL,NULL);
INSERT INTO sessions VALUES ('s_empty','telegram','42',NULL,NULL,NULL,1794203000,0,0,NULL,NULL,NULL);
INSERT INTO messages (session_id,role,content,timestamp) VALUES ('s_recent','user','what is my routine?',1794203279.0);
INSERT INTO messages (session_id,role,content,tool_calls,timestamp) VALUES ('s_recent','assistant','',
 '[{"id":"c1","function":{"name":"tool_call","arguments":"{\"calls\":[{\"name\":\"mcp__chhaya__memory_list\",\"arguments\":{}}]}"}}]',1794203283.9);
INSERT INTO messages (session_id,role,content,tool_call_id,tool_name,timestamp) VALUES ('s_recent','tool','listing','c1','mcp__chhaya__memory_list',1794203283.95);
INSERT INTO messages (session_id,role,content,finish_reason,timestamp) VALUES ('s_recent','assistant','One 10km run per week.','stop',1794203286.8);
INSERT INTO messages (session_id,role,content,timestamp) VALUES ('s_old','user','ancient',1700000000.0);
`

// 1794203283.95 is 05:48:03.95 UTC.
const auditLines = `{"seq":1,"ts":"2026-11-09T05:48:03.946466352Z","actor":"hermes","tool":"memory.list","kind":"read","tier":0,"args":{},"decision":"allow","rule":"tier0.allow","reason":"ok","result":{"status":"ok"},"duration_ms":9}
{"seq":2,"ts":"2026-11-09T05:48:05.1Z","actor":"hermes","tool":"memory.get","kind":"read","tier":0,"args":{"path":"x"},"decision":"allow","rule":"tier0.allow","duration_ms":3}
{"seq":3,"ts":"2026-11-09T05:48:30Z","actor":"approver","tool":"memory.apply","kind":"write","tier":0,"decision":"allow","duration_ms":1}
`

type call struct {
	name string
	args []string
}

func fakeRun(calls *[]call, failOn string) Runner {
	return func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, args})
		if failOn != "" && strings.Contains(strings.Join(args, " "), failOn) {
			return nil, errors.New("access denied")
		}
		return nil, nil
	}
}

func readTrace(t *testing.T, path string) Trace {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	var tr Trace
	must(t, json.Unmarshal(b, &tr))
	return tr
}

func TestExportBuildsTraceWithMatchedAndFlaggedGatewayCalls(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	out := t.TempDir()
	now := time.Unix(1794203400, 0)
	res, err := Export(ExportOptions{Home: home, OutDir: out, Since: 48 * time.Hour, Now: now})
	must(t, err)
	if len(res.Written) != 1 {
		t.Fatalf("written %v; want only the recent session (old is out of the window, empty has no messages)", res.Written)
	}
	want := filepath.Join(out, "days", "2026-11-09", "trace-s_recent.json")
	if res.Written[0] != want {
		t.Fatalf("path = %s, want %s", res.Written[0], want)
	}
	tr := readTrace(t, want)
	if tr.Day != "2026-11-09" || tr.Source != "telegram" || tr.MessageCount != 4 || tr.CostUSD != 0.5 {
		t.Errorf("header wrong: %+v", tr)
	}
	if tr.SystemPromptHash != "hash-abc" {
		t.Errorf("hash = %q", tr.SystemPromptHash)
	}
	tool := tr.Messages[2]
	if tool.Gateway == nil || tool.Gateway.Seq != 1 || tool.Gateway.Decision != "allow" {
		t.Errorf("tool message not joined to audit seq 1: %+v", tool.Gateway)
	}
	if got := tr.Messages[1].Calls; len(got) != 1 || got[0].Name != "mcp__chhaya__memory_list" || got[0].Via != "tool_call" {
		t.Errorf("assistant call not unwrapped: %+v", got)
	}
	if len(tr.Unmatched) != 1 || tr.Unmatched[0].Seq != 2 {
		t.Errorf("seq 2 (memory.get with no tool message) must be flagged: %+v", tr.Unmatched)
	}
	if len(tr.Events) != 1 || tr.Events[0].Actor != "approver" {
		t.Errorf("approver event missing: %+v", tr.Events)
	}
	if res.Flagged != 1 {
		t.Errorf("Flagged = %d", res.Flagged)
	}
	if tr.Messages[3].FinishReason != "stop" || tr.Messages[1].FinishReason != "" {
		t.Errorf("finish_reason must be carried per message: %q / %q", tr.Messages[3].FinishReason, tr.Messages[1].FinishReason)
	}
	if tr.Counters.GatewayCalls["memory.list"] != 1 || tr.Counters.Questions != 0 {
		t.Errorf("counters: %+v", tr.Counters)
	}
}

func TestExportAllIncludesOldSessions(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	res, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), Since: 0, Now: time.Unix(1794203400, 0)})
	must(t, err)
	if len(res.Written) != 2 {
		t.Fatalf("written %v, want recent and old", res.Written)
	}
}

func TestExportDoesNotWriteTheSystemPrompt(t *testing.T) {
	home := fixtureHome(t, schema,
		data+"ALTER TABLE sessions ADD COLUMN system_prompt TEXT; UPDATE sessions SET system_prompt='SECRET-PROMPT-TEXT';", auditLines)
	out := t.TempDir()
	res, err := Export(ExportOptions{Home: home, OutDir: out, Since: 0, Now: time.Unix(1794203400, 0)})
	must(t, err)
	for _, p := range res.Written {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "SECRET-PROMPT-TEXT") {
			t.Fatalf("%s contains the system prompt", p)
		}
	}
}

func TestExportUploadsEachTraceBesideItsDay(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	var calls []call
	res, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), S3: "s3://bkt/evals/", Since: 48 * time.Hour,
		Now: time.Unix(1794203400, 0), Run: fakeRun(&calls, "")})
	must(t, err)
	if res.Uploaded != 1 || len(calls) != 1 {
		t.Fatalf("uploaded %d, calls %v", res.Uploaded, calls)
	}
	a := calls[0].args
	if calls[0].name != "aws" || a[len(a)-1] != "s3://bkt/evals/days/2026-11-09/trace-s_recent.json" {
		t.Errorf("upload call = %+v", calls[0])
	}
}

func TestExportKeepsGoingAfterAFailedUploadAndReportsIt(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	var calls []call
	res, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), S3: "s3://bkt/evals", Since: 0,
		Now: time.Unix(1794203400, 0), Run: fakeRun(&calls, "trace-s_old")})
	if err == nil || !strings.Contains(err.Error(), "s_old") {
		t.Fatalf("err = %v; a failed upload must be reported", err)
	}
	if len(calls) != 2 || res.Uploaded != 1 || len(res.Written) != 2 {
		t.Fatalf("one bad upload must not stop the other: calls=%d uploaded=%d written=%d", len(calls), res.Uploaded, len(res.Written))
	}
}

func TestExportFailsLoudlyWhenAColumnIsGone(t *testing.T) {
	// A Hermes upgrade that renames a column must stop the export, not produce empty traces.
	broken := strings.Replace(schema, "tool_name TEXT,", "tool_label TEXT,", 1)
	home := fixtureHome(t, broken, `INSERT INTO sessions VALUES ('s','telegram','1',NULL,NULL,NULL,1794203279,0,0,NULL,NULL,NULL);
INSERT INTO messages (session_id,role,content,timestamp) VALUES ('s','user','hi',1794203279.0);`, auditLines)
	_, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), Now: time.Unix(1794203400, 0)})
	if err == nil || !strings.Contains(err.Error(), "tool_name") {
		t.Fatalf("err = %v; want the missing column named", err)
	}
}

func TestExportRefusesAnAuditLogThatIsMissingOrCorrupt(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	must(t, os.Remove(filepath.Join(home, "gateway", "audit.jsonl")))
	if _, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), Now: time.Now()}); err == nil {
		t.Fatal("no audit log must fail the export")
	}
	must(t, os.WriteFile(filepath.Join(home, "gateway", "audit.jsonl"), []byte("{oops\n"), 0o600))
	if _, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), Now: time.Now()}); err == nil {
		t.Fatal("a corrupt audit log must fail the export")
	}
}

func TestReadSessionsRefusesAnIDThatWouldEscapeAFolder(t *testing.T) {
	for _, id := range []string{"../evil", "a/b", "a b", "x'; DROP TABLE sessions;--"} {
		q := func(db, sql string) ([]byte, error) {
			return []byte(fmt.Sprintf(`[{"id":%q,"source":"telegram","started_at":1794203279,"last_ts":1794203280}]`, id)), nil
		}
		if _, err := ReadSessions("db", time.Time{}, q); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
}

func TestExportWritesPrivateFiles(t *testing.T) {
	home := fixtureHome(t, schema, data, auditLines)
	res, err := Export(ExportOptions{Home: home, OutDir: t.TempDir(), Since: 0, Now: time.Unix(1794203400, 0)})
	must(t, err)
	fi, err := os.Stat(res.Written[0])
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("trace file mode %v; traces hold private conversations", fi.Mode().Perm())
	}
}
