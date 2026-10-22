package mcpsrv

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

// TestRegistryIsUnreachableExceptThroughInvoke is the structural assertion the
// spec asks for. It reads this package's own source rather than its behaviour,
// because the property is "no other exported path exists" and you cannot
// observe the absence of a path by calling things.
func TestRegistryIsUnreachableExceptThroughInvoke(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Any exported function or method whose body mentions the registry field
	// must be Invoke. Registrations() is allowed because it returns metadata
	// with the handlers stripped; the test below proves that.
	const allowed = "Invoke"
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() || fn.Name.Name == allowed || fn.Name.Name == "Registrations" || fn.Name.Name == "New" {
					return true
				}
				var touches bool
				ast.Inspect(fn, func(m ast.Node) bool {
					if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "tools" {
						touches = true
					}
					return true
				})
				if touches {
					t.Errorf("%s: exported %s reaches the registry; only Invoke may",
						filepath.Base(name), fn.Name.Name)
				}
				return true
			})
		}
	}
}

func TestRegistrationsExposesNoHandlers(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{approve: true})
	for _, r := range g.Registrations() {
		if r.Handle != nil {
			t.Errorf("%s: Registrations leaked a callable handler", r.Name)
		}
	}
}

type fakeApprover struct {
	approve bool
	err     error
	calls   int
}

func (f *fakeApprover) Request(ctx context.Context, call ToolCall, d policy.Decision) (bool, time.Duration, error) {
	f.calls++
	return f.approve, 1500 * time.Millisecond, f.err
}

func newTestGateway(t *testing.T, ap Approver) *Gateway {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	cfg := policy.Config{
		Caps:               map[string]policy.Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
		DefaultRatePerHour: 60,
		ApprovalTimeout:    time.Minute,
		BranchPrefix:       "agent/",
	}

	var called int
	regs := []Registration{
		{
			Name: "probe.read", Description: "test read", Tier: policy.TierInternal,
			Kind: policy.Read, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, map[string]any) (any, error) { called++; return "read-ok", nil },
		},
		{
			Name: "probe.write", Description: "test write", Tier: policy.TierInternal,
			Kind: policy.Write, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, map[string]any) (any, error) { called++; return "write-ok", nil },
		},
		{
			Name: "probe.untrusted", Description: "returns untrusted content", Tier: policy.TierInternal,
			Kind: policy.Read, Trust: Untrusted, Schema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, map[string]any) (any, error) { return "from the internet", nil },
		},
	}

	g, err := New(cfg, st, w, ap, regs)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC) }
	return g
}

func TestUnknownToolIsRefusedAndAudited(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{})
	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "rm.rf"})
	if !res.IsError || res.RuleID != "tool.unknown" {
		t.Fatalf("got %+v", res)
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Decision != "deny" || r.Rule != "tool.unknown" {
			t.Errorf("record = %+v", r)
		}
	})
}

func TestKillSwitchBlocksWriteButNotRead(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{})
	if err := g.state.SetKillSwitch(true, g.now()); err != nil {
		t.Fatal(err)
	}

	w := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !w.IsError || w.RuleID != "killswitch.active" {
		t.Fatalf("write while stopped: %+v", w)
	}
	r := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.read"})
	if r.IsError {
		t.Fatalf("read while stopped should still work: %+v", r)
	}
}

func TestTrustComesFromRegistrationNotTheHandler(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{})
	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.read"})
	if res.Trust != Trusted {
		t.Fatalf("trust = %q", res.Trust)
	}
}

func TestAnUntrustedResultSetsTaint(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{})
	if g.state.Snapshot(g.now()).Tainted {
		t.Fatal("precondition")
	}

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.untrusted"})
	if res.Trust != Untrusted {
		t.Fatalf("trust = %q", res.Trust)
	}
	if !g.state.Snapshot(g.now()).Tainted {
		t.Fatal("an untrusted result must set taint as a side effect of returning")
	}

	// And the next write is escalated, not allowed.
	w := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if w.RuleID != "taint.write" {
		t.Fatalf("expected the write to be escalated by taint, got %+v", w)
	}
}

func TestApprovalFailureIsCriticalAndRefuses(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{err: errUnreachable})
	if err := g.state.SetTaint(true, "probe.untrusted", g.now()); err != nil {
		t.Fatal(err)
	}

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !res.IsError {
		t.Fatal("an unreachable approver must refuse, never allow")
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Severity != "critical" {
			t.Errorf("severity = %q, want critical", r.Severity)
		}
		if r.Decision != "deny" {
			t.Errorf("decision = %q", r.Decision)
		}
	})
}

func TestApprovalWritesTwoRecordsWithLatency(t *testing.T) {
	ap := &fakeApprover{approve: true}
	g := newTestGateway(t, ap)
	g.state.SetTaint(true, "probe.untrusted", g.now())

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if res.IsError {
		t.Fatalf("approved call should run: %+v", res)
	}

	recs := readRecords(t, g)
	last2 := recs[len(recs)-2:]
	if last2[0].Decision != "needs_approval" {
		t.Errorf("first record = %q, want needs_approval", last2[0].Decision)
	}
	if last2[1].Approval == nil || last2[1].Approval.Answer != "approved" {
		t.Errorf("second record = %+v", last2[1])
	}
	if last2[1].Approval.LatencyMS != 1500 {
		t.Errorf("latency = %d, want 1500 — design.md tracks approval fatigue by this", last2[1].Approval.LatencyMS)
	}
}

func TestDeniedApprovalDoesNotRunTheHandler(t *testing.T) {
	g := newTestGateway(t, &fakeApprover{approve: false})
	g.state.SetTaint(true, "probe.untrusted", g.now())

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !res.IsError {
		t.Fatal("a denied approval must not run the tool")
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Approval == nil || r.Approval.Answer != "denied_by_user" {
			t.Errorf("record = %+v", r)
		}
	})
}

func TestNewRejectsATierThreeRegistration(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.json"))
	w, _ := audit.Open(filepath.Join(dir, "audit.jsonl"))
	defer w.Close()

	_, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "bank.wire", Tier: policy.TierIrreversible, Kind: policy.Write, Trust: Trusted,
		Schema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, map[string]any) (any, error) { return nil, nil },
	}})
	if err == nil {
		t.Fatal("a tier-3 tool must not be registerable at all")
	}
}

func TestNewRejectsAnIncompleteRegistration(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.json"))
	w, _ := audit.Open(filepath.Join(dir, "audit.jsonl"))
	defer w.Close()

	cases := map[string]Registration{
		"no trust":   {Name: "a", Tier: policy.TierInternal, Kind: policy.Read, Schema: json.RawMessage(`{"type":"object"}`), Handle: noopHandler},
		"no handler": {Name: "b", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`)},
		"no schema":  {Name: "c", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted, Handle: noopHandler},
		"no name":    {Name: "", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`), Handle: noopHandler},
	}
	for name, reg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{reg}); err == nil {
				t.Fatal("an incomplete registration is a bug, not a default")
			}
		})
	}
}

func noopHandler(context.Context, map[string]any) (any, error) { return nil, nil }

func assertLastRecord(t *testing.T, g *Gateway, check func(audit.Record)) {
	t.Helper()
	recs := readRecords(t, g)
	if len(recs) == 0 {
		t.Fatal("no audit records were written")
	}
	check(recs[len(recs)-1])
}

func readRecords(t *testing.T, g *Gateway) []audit.Record {
	t.Helper()
	b, err := os.ReadFile(g.auditPath)
	if err != nil {
		t.Fatal(err)
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
