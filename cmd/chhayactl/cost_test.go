package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/cost"
)

type recSender struct {
	token string
	chat  int64
	got   []string
}

func (r *recSender) Send(_ context.Context, text string) error {
	r.got = append(r.got, text)
	return nil
}

func costFixture(t *testing.T) (home, env string, d costDeps, rs *recSender) {
	t.Helper()
	home = t.TempDir()
	os.MkdirAll(filepath.Join(home, "gateway"), 0o700)
	env = filepath.Join(t.TempDir(), ".env.gateway")
	os.WriteFile(env, []byte("CHHAYA_APPROVER_BOT_TOKEN=tok-123\nCHHAYA_APPROVER_ALLOWED_USERS=42\n"), 0o600)
	now := time.Unix(2_000_000_000, 0)
	old := float64(now.Add(-2 * time.Hour).Unix())
	rs = &recSender{}
	d = costDeps{
		now: func() time.Time { return now },
		query: func(db, sql string) ([]byte, error) {
			return []byte(`[{"id":"s1","source":"telegram","model":"claude-haiku-5-5","started_at":` + itoa(old-600) + `,"ended_at":` + itoa(old) + `,
			"last_activity_at":` + itoa(old) + `,"last_msg":` + itoa(old) + `,"message_count":4,"tool_call_count":1,"input_tokens":1000000,"output_tokens":100000,
			"cache_read_tokens":0,"cache_write_tokens":0,"api_call_count":20}]`), nil
		},
		sender: func(token string, chat int64) cost.Sender { rs.token, rs.chat = token, chat; return rs },
	}
	return
}

func itoa(f float64) string { return strconv.FormatInt(int64(f), 10) }

func TestCostReportPrintsAndDoesNotSendWithoutFlag(t *testing.T) {
	home, env, d, rs := costFixture(t)
	var out bytes.Buffer
	if err := runCost([]string{"report", "-home", home, "-prices", "../../config/pricing.yaml", "-env", env}, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(rs.got) != 0 || !strings.Contains(out.String(), "would be sent") || !strings.Contains(out.String(), "$0.150") {
		t.Fatalf("%q sent=%v", out.String(), rs.got)
	}
}

func TestCostReportSendsOnceThenSaysNothingNew(t *testing.T) {
	home, env, d, rs := costFixture(t)
	args := []string{"report", "-send", "-q", "-home", home, "-prices", "../../config/pricing.yaml", "-env", env}
	var out bytes.Buffer
	if err := runCost(args, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(rs.got) != 1 || rs.token != "tok-123" || rs.chat != 42 || !strings.Contains(rs.got[0], "$0.150") {
		t.Fatalf("sent=%v token=%q chat=%d", rs.got, rs.token, rs.chat)
	}
	if strings.Contains(out.String(), "tok-123") || strings.Contains(rs.got[0], "tok-123") {
		t.Fatal("the bot token must never be printed or sent")
	}
	if _, err := os.Stat(filepath.Join(home, "gateway", "cost-reported.json")); err != nil {
		t.Fatal("the report must be remembered:", err)
	}
	out.Reset()
	if err := runCost(args, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(rs.got) != 1 || !strings.Contains(out.String(), "nothing new") {
		t.Fatalf("sent twice: %v / %q", rs.got, out.String())
	}
}

func TestCostReportNeedsTheBotSettings(t *testing.T) {
	home, env, d, _ := costFixture(t)
	os.WriteFile(env, []byte("CHHAYA_APPROVER_BOT_TOKEN=tok\n"), 0o600)
	err := runCost([]string{"report", "-send", "-home", home, "-prices", "../../config/pricing.yaml", "-env", env}, &bytes.Buffer{}, d)
	if err == nil || strings.Contains(err.Error(), "tok") && !strings.Contains(err.Error(), "needs") {
		t.Fatalf("expected a clear refusal, got %v", err)
	}
}
