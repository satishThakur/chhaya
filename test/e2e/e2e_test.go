//go:build e2e

// Package e2e is the M0b exit check: a blocked write is logged.
// Run with: go test -tags e2e ./test/e2e/...
package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
	"github.com/satishthakur/chhaya/internal/tools"
)

// TestExitCheckBlockedWriteIsLogged is the one flow M0b exists to prove:
//
//	/stop on the approver bot
//	  → memory.propose_update
//	  → Deny(killswitch.active)
//	  → an audit record exists
//	  → chhayactl audit verify passes
//
// It uses only real tools. There is no debug.write_probe, because a probe that
// exists only for the test proves only that the probe works.
func TestExitCheckBlockedWriteIsLogged(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	bin := build(t, "chhayactl")

	// Step 1: /stop, through the path that does not need the daemon.
	run(t, bin, "-data", dir, "stop")

	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("stop did not persist state: %v", err)
	}
	if !strings.Contains(string(b), `"kill_switch":true`) {
		t.Fatalf("state = %s", b)
	}

	// Step 2: the blocked write, through Invoke. We exercise the gateway
	// in-process here rather than over MCP; the mTLS leg is asserted
	// separately in TestClientWithoutACertificateIsRefused.
	logPath := filepath.Join(dir, "audit.jsonl")
	res := invokeProposeUpdate(t, dir)
	if !res.IsError {
		t.Fatal("a write while stopped was allowed")
	}
	if res.RuleID != "killswitch.active" {
		t.Fatalf("rule = %q, want killswitch.active", res.RuleID)
	}

	// Step 3: the record exists and says what happened.
	recs := readLog(t, logPath)
	if len(recs) == 0 {
		t.Fatal("the refusal left no trace, which is the one thing M0b must not do")
	}
	last := recs[len(recs)-1]
	switch {
	case last.Tool != "memory.propose_update":
		t.Errorf("tool = %q", last.Tool)
	case last.Decision != "deny":
		t.Errorf("decision = %q", last.Decision)
	case last.Rule != "killswitch.active":
		t.Errorf("rule = %q", last.Rule)
	case last.Hash == "":
		t.Error("the record is not chained")
	}

	// Step 4: the chain verifies, from a different process.
	out := run(t, bin, "-data", dir, "audit", "verify")
	if !strings.Contains(out, "chain intact") {
		t.Fatalf("verify said: %s", out)
	}
}

func build(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "../../cmd/"+name)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

func run(t *testing.T, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, b)
	}
	return string(b)
}

func readLog(t *testing.T, path string) []audit.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no audit log at %s: %v", path, err)
	}
	var out []audit.Record
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

type refusingApprover struct{}

func (refusingApprover) Request(context.Context, mcpsrv.ToolCall, policy.Decision) (bool, time.Duration, error) {
	return false, 0, nil
}

func invokeProposeUpdate(t *testing.T, dir string) mcpsrv.ToolResult {
	t.Helper()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	memRoot := filepath.Join(dir, "memory")
	os.MkdirAll(filepath.Join(memRoot, "taste"), 0o700)
	os.WriteFile(filepath.Join(memRoot, "taste", "food.md"), []byte("# Food\n"), 0o600)

	cfg := policy.Config{
		Caps:               map[string]policy.Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
		DefaultRatePerHour: 60, ApprovalTimeout: time.Minute, BranchPrefix: "agent/",
	}

	var g *mcpsrv.Gateway
	regs := tools.All(tools.Deps{
		Version: "e2e", MemoryRoot: memRoot, State: st, Config: cfg,
		Describe: func() []mcpsrv.Registration { return g.Registrations() },
	})
	g, err = mcpsrv.New(cfg, st, w, refusingApprover{}, regs)
	if err != nil {
		t.Fatal(err)
	}

	return g.Invoke(context.Background(), mcpsrv.ToolCall{
		Actor: "hermes", Name: "memory.propose_update",
		Args: map[string]any{"path": "taste/food", "entry": "nothing", "rationale": "exit check"},
	})
}

// TestClientWithoutACertificateIsRefused is the mTLS negative control. It is
// the half of "mTLS works" that is easy to forget to assert: that the door is
// actually locked, not just that your key opens it.
//
// Certs are required, not skipped-if-missing, for the same reason as in
// TestBlockedWriteOverRealMTLS below: a skipped test reports green. The two
// had opposite answers to the same precondition in the same file.
func TestClientWithoutACertificateIsRefused(t *testing.T) {
	certDir := "../../deploy/certs"
	if _, err := os.Stat(filepath.Join(certDir, "ca.crt")); err != nil {
		t.Fatal("certs are required for this test; run deploy/gen-certs.sh")
	}
	tlsCfg, err := mcpsrv.TLSConfig(
		filepath.Join(certDir, "gateway.crt"),
		filepath.Join(certDir, "gateway.key"),
		filepath.Join(certDir, "ca.crt"),
	)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	// No client certificate: must fail at the handshake.
	bare := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	if _, err := bare.Get(srv.URL); err == nil {
		t.Fatal("a client with no certificate got in")
	}

	// With Hermes' certificate: must succeed.
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "hermes.crt"), filepath.Join(certDir, "hermes.key"))
	if err != nil {
		t.Fatal(err)
	}
	good := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true},
	}}
	resp, err := good.Get(srv.URL)
	if err != nil {
		t.Fatalf("Hermes' own certificate was refused: %v", err)
	}
	resp.Body.Close()
}

// TestBlockedWriteOverRealMTLS is the exit check through the door Hermes
// actually knocks on. TestExitCheckBlockedWriteIsLogged proves the policy and
// the audit record in-process; TestClientWithoutACertificateIsRefused proves
// the transport enforces mTLS against a bare handler. Neither proves the two
// assembled: that a blocked write still ends up denied-with-an-audit-record
// when it arrives as a real MCP tool call, over mTLS, through
// Gateway.MCPHandler. This test assembles them.
//
// Certs are required, not skipped-if-missing: a skipped test reports green,
// and this is the one test that must not quietly pass by not running.
func TestBlockedWriteOverRealMTLS(t *testing.T) {
	certDir := "../../deploy/certs"
	if _, err := os.Stat(filepath.Join(certDir, "ca.crt")); err != nil {
		t.Fatal("certs are required for this test; run deploy/gen-certs.sh")
	}

	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKillSwitch(true, time.Now()); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "audit.jsonl")
	w, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	memRoot := filepath.Join(dir, "memory")
	if err := os.MkdirAll(filepath.Join(memRoot, "taste"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memRoot, "taste", "food.md"), []byte("# Food\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := policy.Config{
		Caps:               map[string]policy.Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
		DefaultRatePerHour: 60, ApprovalTimeout: time.Minute, BranchPrefix: "agent/",
	}

	var g *mcpsrv.Gateway
	regs := tools.All(tools.Deps{
		Version: "e2e", MemoryRoot: memRoot, State: st, Config: cfg,
		Describe: func() []mcpsrv.Registration { return g.Registrations() },
	})
	g, err = mcpsrv.New(cfg, st, w, refusingApprover{}, regs)
	if err != nil {
		t.Fatal(err)
	}

	tlsCfg, err := mcpsrv.TLSConfig(
		filepath.Join(certDir, "gateway.crt"),
		filepath.Join(certDir, "gateway.key"),
		filepath.Join(certDir, "ca.crt"),
	)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", g.MCPHandler("chhaya-gateway", "e2e"))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "hermes.crt"), filepath.Join(certDir, "hermes.key"))
	if err != nil {
		t.Fatal(err)
	}
	// InsecureSkipVerify is acceptable here only because this test exercises
	// the SERVER's enforcement of the client certificate (mTLS), not the
	// client's trust of the server's identity. A real client must verify the
	// server; do not copy this flag into production code.
	httpc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: httpc,
	}, nil)
	if err != nil {
		t.Fatalf("connect over mTLS: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "memory.propose_update",
		Arguments: map[string]any{"path": "taste/food", "entry": "nothing", "rationale": "exit check over mTLS"},
	})
	if err != nil {
		t.Fatalf("the refusal must arrive as a tool error the model can see, not a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("a write while stopped was allowed, over mTLS")
	}
	if text := contentText(res); !strings.Contains(text, "kill switch") {
		t.Fatalf("tool error = %q, want it to mention the kill switch", text)
	}

	recs := readLog(t, logPath)
	if len(recs) == 0 {
		t.Fatal("the refusal over mTLS left no trace")
	}
	last := recs[len(recs)-1]
	if last.Decision != "deny" || last.Rule != "killswitch.active" {
		t.Fatalf("decision=%q rule=%q, want deny/killswitch.active", last.Decision, last.Rule)
	}

	if entries, err := os.ReadDir(filepath.Join(memRoot, ".proposals")); err == nil && len(entries) > 0 {
		t.Fatalf("a proposal file was written while the kill switch was on: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func contentText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
