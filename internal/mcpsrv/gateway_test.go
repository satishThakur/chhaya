package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
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
// spec asks for. It reads this package's own source rather than its
// behaviour, because the property is "no other exported path exists" and you
// cannot observe the absence of a path by calling things.
//
// What this does NOT catch: an exported method that reaches a handler
// indirectly, through an unexported helper that itself touches g.tools rather
// than touching it directly. The walk only flags a SelectorExpr naming
// "tools" inside the exported function's own body, not inside something it
// calls. That gap is accepted because closing it fully means re-implementing
// a call graph, and the directness of this package — few functions, no deep
// call chains — is what makes the gap small in practice, not the test.
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
	// with the handlers stripped; the test below proves that. MCPHandler is
	// allowed because it ranges over g.tools only to publish each tool's name,
	// description and schema to the SDK — TestMCPHandlerDoesNotCallHandlersDirectly
	// is the compensating check that it never reaches a handler directly.
	const allowed = "Invoke"
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() || fn.Name.Name == allowed || fn.Name.Name == "Registrations" || fn.Name.Name == "New" || fn.Name.Name == "MCPHandler" {
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
	g, _ := newTestGateway(t, &fakeApprover{approve: true})
	for _, r := range g.Registrations() {
		if r.Handle != nil {
			t.Errorf("%s: Registrations leaked a callable handler", r.Name)
		}
		if r.Validate != nil {
			t.Errorf("%s: Registrations leaked a validator", r.Name)
		}
	}
}

func TestRegistrationsIsSortedByName(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{approve: true})
	regs := g.Registrations()
	for i := 1; i < len(regs); i++ {
		if regs[i-1].Name > regs[i].Name {
			t.Fatalf("Registrations is not sorted: %q before %q", regs[i-1].Name, regs[i].Name)
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

// approverFunc adapts a plain function to Approver, the way http.HandlerFunc
// adapts a function to http.Handler. It exists so a test can make the
// approver take an action (like flipping the kill switch) as part of
// answering, without a dedicated named type per test.
type approverFunc func(ctx context.Context, call ToolCall, d policy.Decision) (bool, time.Duration, error)

func (f approverFunc) Request(ctx context.Context, call ToolCall, d policy.Decision) (bool, time.Duration, error) {
	return f(ctx, call, d)
}

// newTestGateway returns the gateway plus a pointer to a counter incremented
// by every probe handler that runs. Tests that need to prove a handler did or
// did not execute — not just that Invoke returned the shape they expected —
// read through this pointer.
func newTestGateway(t *testing.T, ap Approver) (*Gateway, *int) {
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

	called := new(int)
	regs := []Registration{
		{
			Name: "probe.read", Description: "test read", Tier: policy.TierInternal,
			Kind: policy.Read, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, map[string]any) (any, error) { *called++; return "read-ok", nil },
		},
		{
			Name: "probe.write", Description: "test write", Tier: policy.TierInternal,
			Kind: policy.Write, Trust: Trusted, Schema: json.RawMessage(`{"type":"object"}`),
			// A validator on a tier-0 tool is legal — it is just optional.
			// TestValidatorIsConsultedWhenSupplied checks, with its own
			// gateway, that supplying one is not silently ignored outside
			// tier 1; this one only needs to exist so
			// TestRegistrationsExposesNoHandlers has a non-nil Validate to
			// prove gets stripped.
			Validate: func(map[string]any) error { return nil },
			// A declared (non-secretish-named) redaction path, so
			// TestMutatingRegistrationsCannotDisableRedaction has something
			// to try to disable.
			Redact: []string{"redact_me"},
			Handle: func(context.Context, map[string]any) (any, error) { *called++; return "write-ok", nil },
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
	return g, called
}

func TestUnknownToolIsRefusedAndAudited(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{})
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
	g, called := newTestGateway(t, &fakeApprover{})
	if err := g.state.SetKillSwitch(true, g.now()); err != nil {
		t.Fatal(err)
	}

	w := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !w.IsError || w.RuleID != "killswitch.active" {
		t.Fatalf("write while stopped: %+v", w)
	}
	if *called != 0 {
		t.Fatalf("a denied write must not run the handler, called = %d", *called)
	}
	r := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.read"})
	if r.IsError {
		t.Fatalf("read while stopped should still work: %+v", r)
	}
}

func TestTrustComesFromRegistrationNotTheHandler(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{})
	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.read"})
	if res.Trust != Trusted {
		t.Fatalf("trust = %q", res.Trust)
	}
}

func TestAnUntrustedResultSetsTaint(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{})
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

// TestUntrustedHandlerErrorStillSetsTaint is the regression test for the
// CRITICAL finding: an untrusted tool whose handler errors is still untrusted
// content (a fetched page's body, a remote server's message, surfacing
// through err.Error()). Steering an untrusted tool into an error path must
// not be a way to dodge taint.
func TestUntrustedHandlerErrorStillSetsTaint(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	g, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "probe.untrusted.errors", Tier: policy.TierInternal, Kind: policy.Read, Trust: Untrusted,
		Schema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("fetched from an attacker-controlled page")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC) }

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.untrusted.errors"})
	if !res.IsError {
		t.Fatal("expected the handler's error to surface")
	}
	if !g.state.Snapshot(g.now()).Tainted {
		t.Fatal("an untrusted tool's error must still taint the session — an error path must not launder trust")
	}
}

// TestPanickingHandlerStillProducesAnAuditRecord is the regression test for
// the Important finding that a panicking handler unwound past record(),
// taking any side effect with it but leaving no audit trail.
func TestPanickingHandlerStillProducesAnAuditRecord(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	g, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "probe.panics", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted,
		Schema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, map[string]any) (any, error) { panic("boom") },
	}})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC) }

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.panics"})
	if !res.IsError {
		t.Fatal("a panicking handler must surface as an error, not propagate out of Invoke")
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Tool != "probe.panics" {
			t.Errorf("record = %+v", r)
		}
	})
}

func TestApprovalFailureIsCriticalAndRefuses(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{err: errUnreachable})
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
		// The agent is told "approval.unreachable"; the log must say the same
		// thing, not the original rule (e.g. "taint.write") that triggered
		// the approval requirement in the first place.
		if r.Rule != "approval.unreachable" {
			t.Errorf("rule = %q, want approval.unreachable", r.Rule)
		}
	})
}

func TestApprovalWritesTwoRecordsWithLatency(t *testing.T) {
	ap := &fakeApprover{approve: true}
	g, called := newTestGateway(t, ap)
	g.state.SetTaint(true, "probe.untrusted", g.now())

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if res.IsError {
		t.Fatalf("approved call should run: %+v", res)
	}
	if *called != 1 {
		t.Fatalf("an approved call must run the handler exactly once, got %d", *called)
	}

	recs := readRecords(t, g)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want exactly 2 — the request and the answer, not more and not fewer", len(recs))
	}
	if recs[0].Decision != "needs_approval" {
		t.Errorf("first record = %q, want needs_approval", recs[0].Decision)
	}
	if recs[1].Approval == nil || recs[1].Approval.Answer != "approved" {
		t.Errorf("second record = %+v", recs[1])
	}
	if recs[1].Approval.LatencyMS != 1500 {
		t.Errorf("latency = %d, want 1500 — design.md tracks approval fatigue by this", recs[1].Approval.LatencyMS)
	}
}

func TestDeniedApprovalDoesNotRunTheHandler(t *testing.T) {
	g, called := newTestGateway(t, &fakeApprover{approve: false})
	g.state.SetTaint(true, "probe.untrusted", g.now())

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !res.IsError {
		t.Fatal("a denied approval must not run the tool")
	}
	if *called != 0 {
		t.Fatalf("a denied approval must not run the handler, called = %d", *called)
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Approval == nil || r.Approval.Answer != "denied_by_user" {
			t.Errorf("record = %+v", r)
		}
	})
}

// TestKillSwitchFlippedDuringApprovalStillRefuses is the regression test for
// the Important finding that the kill switch was decided once, before the
// human answered, and never re-checked. /stop pressed while the approval card
// is pending — the most likely moment for a user to press it — must still
// stop the write even though the human went on to approve it.
func TestKillSwitchFlippedDuringApprovalStillRefuses(t *testing.T) {
	var g *Gateway
	ap := approverFunc(func(ctx context.Context, call ToolCall, d policy.Decision) (bool, time.Duration, error) {
		if err := g.state.SetKillSwitch(true, g.now()); err != nil {
			t.Fatal(err)
		}
		return true, 0, nil
	})
	g, called := newTestGateway(t, ap)
	g.state.SetTaint(true, "probe.untrusted", g.now())

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	if !res.IsError || res.RuleID != "killswitch.active" {
		t.Fatalf("expected a re-decided refusal after approval, got %+v", res)
	}
	if *called != 0 {
		t.Fatalf("the handler must not run once the kill switch is active, called = %d", *called)
	}
}

func TestApproverIsNotConsultedOnAnAllowPath(t *testing.T) {
	ap := &fakeApprover{}
	g, _ := newTestGateway(t, ap)
	if res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.read"}); res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if ap.calls != 0 {
		t.Fatalf("the approver must not be consulted on an allow path, calls = %d", ap.calls)
	}
}

// hangingApprover never answers: Request blocks until its context is done and
// then reports that as an error. It exists to prove the backstop in Invoke
// bounds the approver call — nothing else in this approver would ever return.
type hangingApprover struct{ released chan struct{} }

func (h *hangingApprover) Request(ctx context.Context, _ ToolCall, _ policy.Decision) (bool, time.Duration, error) {
	<-ctx.Done()
	return false, 0, ctx.Err()
}

// TestAHungApproverCannotBlockForever is the regression test for the
// Important finding that nothing bounded g.approver.Request: an approver that
// accepts the request and never answers would block Invoke forever, which is
// neither allow nor refuse — the one state "fail closed" exists to exclude.
//
// It shrinks both cfg.ApprovalTimeout and the package-level
// approvalBackstopMargin so the test runs in milliseconds rather than the
// production 30-second margin; approvalBackstopMargin only exists as a
// package var (rather than the literal `30 * time.Second` inline) so this
// test can do that without touching what ships.
func TestAHungApproverCannotBlockForever(t *testing.T) {
	g, _ := newTestGateway(t, &hangingApprover{})
	g.cfg.ApprovalTimeout = 20 * time.Millisecond

	savedMargin := approvalBackstopMargin
	approvalBackstopMargin = 20 * time.Millisecond
	t.Cleanup(func() { approvalBackstopMargin = savedMargin })

	if err := g.state.SetTaint(true, "probe.untrusted", g.now()); err != nil {
		t.Fatal(err)
	}

	done := make(chan ToolResult, 1)
	go func() {
		done <- g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write"})
	}()

	select {
	case res := <-done:
		if !res.IsError {
			t.Fatal("a hung approver must produce a refusal, never an allow")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke did not return — a hung approver blocked forever, which is neither allow nor refuse")
	}
}

// TestSecretishArgsNeverReachTheAuditLog checks CLAUDE.md's "secrets come
// from env only, never log them" at the one place this package touches it:
// the raw bytes record() hands to audit.Append. It reads the log file
// directly rather than the parsed Args, because a bug that only breaks one
// field of a parsed struct is a different, narrower claim than "the secret
// literally never reached disk."
func TestSecretishArgsNeverReachTheAuditLog(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{})
	secret := "sk-do-not-log-this-please"
	g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write", Args: map[string]any{"api_key": secret}})

	b, err := os.ReadFile(g.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("a secret-shaped argument reached the audit log")
	}
}

// TestMutatingRegistrationsCannotDisableRedaction is the regression test for
// the Important finding that Registrations() handed out live aliases of
// Redact and Schema: a caller mutating the slice it got back could silently
// disable a declared redaction path for every later call, since record()
// reads reg.Redact fresh from the registry on each Invoke.
func TestMutatingRegistrationsCannotDisableRedaction(t *testing.T) {
	g, _ := newTestGateway(t, &fakeApprover{approve: true})

	for _, r := range g.Registrations() {
		for i := range r.Redact {
			r.Redact[i] = "not-a-real-path"
		}
		if len(r.Schema) > 0 {
			r.Schema[0] = ' '
		}
	}

	// probe.write declares Redact: []string{"redact_me"} in newTestGateway.
	g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.write",
		Args: map[string]any{"redact_me": "SHOULD-NOT-APPEAR"}})

	raw, err := os.ReadFile(g.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SHOULD-NOT-APPEAR") {
		t.Fatal("mutating the slice returned by Registrations disabled a declared redaction path")
	}
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

// TestNewRejectsATierTwoRegistration is the regression test for the
// Important finding that tier-2 tools are registerable today even though the
// spending caps are not wired into Invoke — AmountMinor is never set and
// state.RecordSpend is never called, so a money tool would be protected by
// the approval card alone.
func TestNewRejectsATierTwoRegistration(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.json"))
	w, _ := audit.Open(filepath.Join(dir, "audit.jsonl"))
	defer w.Close()

	_, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "swiggy.order", Tier: policy.TierMoney, Kind: policy.Write, Trust: Trusted,
		Schema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, map[string]any) (any, error) { return nil, nil },
	}})
	if err == nil {
		t.Fatal("a tier-2 tool must not be registerable until the caps are wired into Invoke")
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

// TestValidatorIsConsultedWhenSupplied is the regression test for the
// Important finding that a validator was only ever consulted for tier-1
// tools: a registration that supplies one for any other tier and never has it
// called is a trap for whoever added it expecting it to run.
func TestValidatorIsConsultedWhenSupplied(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var validated int
	g, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "probe.tier0.validated", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted,
		Schema:   json.RawMessage(`{"type":"object"}`),
		Validate: func(map[string]any) error { validated++; return nil },
		Handle:   func(context.Context, map[string]any) (any, error) { return "ok", nil },
	}})
	if err != nil {
		t.Fatal(err)
	}

	g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.tier0.validated"})
	if validated != 1 {
		t.Fatalf("a supplied validator must be consulted even outside tier 1, consulted %d times", validated)
	}
}

// TestDispatchRefusalIsLoggedAsDenyNotAllow is the regression test for the
// Important finding that a refusal manufactured by dispatch itself (today,
// only state.RecordCall failing) was logged as decision: "allow" with the
// original policy rule, asserting the gateway let through a call it actually
// blocked. It makes the state file's directory unwritable so RecordCall's
// flush fails, forcing dispatch's own state.unwritable path — the audit
// writer keeps its already-open file descriptor and can still append, since
// appending needs write permission on the file, not the directory.
func TestDispatchRefusalIsLoggedAsDenyNotAllow(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	g, err := New(policy.Config{DefaultRatePerHour: 60}, st, w, &fakeApprover{}, []Registration{{
		Name: "probe.allow", Tier: policy.TierInternal, Kind: policy.Read, Trust: Trusted,
		Schema: json.RawMessage(`{"type":"object"}`),
		Handle: func(context.Context, map[string]any) (any, error) { return "ok", nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC) }

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	res := g.Invoke(context.Background(), ToolCall{Actor: "hermes", Name: "probe.allow"})
	if !res.IsError || res.RuleID != "state.unwritable" {
		t.Fatalf("got %+v", res)
	}
	assertLastRecord(t, g, func(r audit.Record) {
		if r.Decision != "deny" || r.Rule != "state.unwritable" {
			t.Errorf("record = %+v, want decision=deny rule=state.unwritable", r)
		}
	})
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
