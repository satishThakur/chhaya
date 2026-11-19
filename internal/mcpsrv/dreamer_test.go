package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

// dreamerGateway has stand-ins named like the real memory tools, plus tools the dreamer must never
// reach. calls counts every handler that ran, by name.
func dreamerGateway(t *testing.T, proposalFails *bool) (*Gateway, map[string]int) {
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
	calls := map[string]int{}
	mk := func(name string, kind policy.Kind, trust Trust) Registration {
		return Registration{Name: name, Description: name, Tier: policy.TierInternal, Kind: kind, Trust: trust,
			Schema: json.RawMessage(`{"type":"object"}`),
			Handle: func(context.Context, map[string]any) (any, error) {
				calls[name]++
				if name == "memory.propose_update" && proposalFails != nil && *proposalFails {
					return nil, errors.New("entry rejected")
				}
				return name + " ok", nil
			}}
	}
	regs := []Registration{
		mk("memory.list", policy.Read, Trusted), mk("memory.get", policy.Read, Trusted),
		mk("memory.propose_update", policy.Write, Trusted),
		mk("capabilities", policy.Read, Trusted),
		mk("probe.write", policy.Write, Trusted), mk("probe.untrusted", policy.Read, Untrusted),
	}
	cfg := policy.Config{
		DefaultRatePerHour: 60, ApprovalTimeout: time.Minute, BranchPrefix: "agent/",
		Dreamer: policy.DreamerLimits{ProposalsPerDay: 5, CallsPerHour: 600},
	}
	g, err := New(cfg, st, w, &fakeApprover{}, regs)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return time.Date(2026, 11, 19, 21, 10, 0, 0, time.UTC) }
	return g, calls
}

func call(g *Gateway, actor, tool string) ToolResult {
	return g.Invoke(context.Background(), ToolCall{Actor: actor, Name: tool, Args: map[string]any{}})
}

func TestDreamerCanReadMemoryAndProposeAndIsAuditedAsTheDreamer(t *testing.T) {
	g, calls := dreamerGateway(t, nil)
	for _, tool := range []string{"memory.list", "memory.get", "memory.propose_update"} {
		if r := call(g, "dreamer", tool); r.IsError {
			t.Fatalf("%s: %+v", tool, r)
		}
	}
	for _, rec := range readRecords(t, g) {
		if rec.Actor != "dreamer" || rec.Decision != "allow" {
			t.Errorf("%+v", rec)
		}
	}
	if calls["memory.propose_update"] != 1 {
		t.Fatalf("%v", calls)
	}
}

func TestDreamerIsRefusedEveryOtherToolAndTheRefusalIsAudited(t *testing.T) {
	g, calls := dreamerGateway(t, nil)
	for _, tool := range []string{"capabilities", "probe.write", "probe.untrusted", "memory.merge", "rm.rf"} {
		r := call(g, "dreamer", tool)
		want := "actor.tool_not_allowed"
		if tool == "rm.rf" || tool == "memory.merge" {
			want = "tool.unknown"
		}
		if !r.IsError || r.RuleID != want {
			t.Errorf("%s: %+v", tool, r)
		}
		assertLastRecord(t, g, func(rec audit.Record) {
			if rec.Actor != "dreamer" || rec.Decision != "deny" || rec.Rule != want {
				t.Errorf("%s: %+v", tool, rec)
			}
		})
	}
	if calls["capabilities"]+calls["probe.write"]+calls["probe.untrusted"] != 0 {
		t.Fatalf("a refused tool's handler ran: %v", calls)
	}
	if g.state.Snapshot(g.now()).Tainted {
		t.Fatal("the dreamer reached an untrusted tool and tainted the session")
	}
}

func TestDreamerSixthProposalOfTheDayIsRefusedAndAFailedOneDoesNotCount(t *testing.T) {
	fail := true
	g, calls := dreamerGateway(t, &fail)
	for i := 0; i < 3; i++ {
		if r := call(g, "dreamer", "memory.propose_update"); !r.IsError {
			t.Fatalf("the handler was set to fail: %+v", r)
		}
	}
	if n := g.state.Snapshot(g.now()).Spent[policy.CapDreamerProposals]; n != 0 {
		t.Fatalf("a proposal the owner never saw was counted: %d", n)
	}
	fail = false
	for i := 0; i < 5; i++ {
		if r := call(g, "dreamer", "memory.propose_update"); r.IsError {
			t.Fatalf("proposal %d: %+v", i+1, r)
		}
	}
	before := calls["memory.propose_update"]
	r := call(g, "dreamer", "memory.propose_update")
	if !r.IsError || r.RuleID != "dreamer.cap.proposals" || calls["memory.propose_update"] != before {
		t.Fatalf("the sixth must be refused before the handler: %+v calls=%v", r, calls)
	}
	// The next day starts again.
	g.now = func() time.Time { return time.Date(2026, 11, 20, 21, 10, 0, 0, time.UTC) }
	if r := call(g, "dreamer", "memory.propose_update"); r.IsError {
		t.Fatalf("a new day: %+v", r)
	}
}

func TestHermesIsNotLimitedByTheDreamersCapAndDoesNotUseIt(t *testing.T) {
	g, _ := dreamerGateway(t, nil)
	for i := 0; i < 8; i++ {
		if r := call(g, "hermes", "memory.propose_update"); r.IsError {
			t.Fatalf("hermes proposal %d: %+v", i+1, r)
		}
	}
	if n := g.state.Snapshot(g.now()).Spent[policy.CapDreamerProposals]; n != 0 {
		t.Fatalf("hermes' proposals were counted against the dreamer: %d", n)
	}
	if r := call(g, "dreamer", "memory.propose_update"); r.IsError {
		t.Fatalf("the dreamer's cap is untouched by hermes: %+v", r)
	}
}

func TestAnUnknownOrEmptyActorIsRefusedAndStillAudited(t *testing.T) {
	g, calls := dreamerGateway(t, nil)
	for _, a := range []string{"", "intruder", "Dreamer"} {
		r := call(g, a, "memory.get")
		if !r.IsError || r.RuleID != "actor.unknown" || calls["memory.get"] != 0 {
			t.Errorf("actor %q: %+v", a, r)
		}
	}
	if len(readRecords(t, g)) != 3 {
		t.Fatal("every refusal must be recorded")
	}
}

func TestTaintDoesNotMakeADreamerProposalNeedASecondApproval(t *testing.T) {
	g, _ := dreamerGateway(t, nil)
	if err := g.state.SetTaint(true, "web.fetch", g.now()); err != nil {
		t.Fatal(err)
	}
	ap := g.approver.(*fakeApprover)
	if r := call(g, "dreamer", "memory.propose_update"); r.IsError || ap.calls != 0 {
		t.Fatalf("%+v approver asked %d times", r, ap.calls)
	}
	if r := call(g, "hermes", "memory.propose_update"); ap.calls != 1 || !r.IsError {
		t.Fatalf("hermes must still need approval while tainted (the fake approver declines): %+v calls=%d", r, ap.calls)
	}
}
