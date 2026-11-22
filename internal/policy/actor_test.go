package policy

import (
	"strings"
	"testing"
)

func dreamerCfg() Config {
	c := baseCfg()
	c.Dreamer = DreamerLimits{ProposalsPerDay: 5, CallsPerHour: 300}
	return c
}

func dreq(tool string, kind Kind) Request {
	r := req(tool, TierInternal, kind)
	r.Actor = ActorDreamer
	return r
}

func TestDreamerMayOnlyReadMemoryAndPropose(t *testing.T) {
	cfg := dreamerCfg()
	for _, tool := range []string{"memory.list", "memory.get"} {
		if d := Decide(dreq(tool, Read), State{}, cfg, now); d.Outcome != Allow {
			t.Errorf("%s: %+v", tool, d)
		}
	}
	if d := Decide(dreq("memory.propose_update", Write), State{}, cfg, now); d.Outcome != Allow {
		t.Errorf("propose: %+v", d)
	}
	// Everything else the gateway has, or ever will have, is refused for this actor, by name.
	for _, tool := range []string{"capabilities", "ping", "swiggy.order", "claude_worker.run", "garmin.read", "web.fetch",
		"memory.write", "memory.propose_remove", "health.get_summary", "health.get_series", "health.get_digest", "memory.merge", "memory.revert", "memory.approve", "memory.delete", "memory.put_test", ""} {
		d := Decide(dreq(tool, Read), State{}, cfg, now)
		if d.Outcome != Deny || d.RuleID != "actor.tool_not_allowed" {
			t.Errorf("%q must be refused for the dreamer: %+v", tool, d)
		}
		d = Decide(dreq(tool, Write), State{}, cfg, now)
		if d.Outcome != Deny || d.RuleID != "actor.tool_not_allowed" {
			t.Errorf("%q (write) must be refused for the dreamer: %+v", tool, d)
		}
	}
}

func TestUnknownOrMissingActorIsDenied(t *testing.T) {
	for _, a := range []string{"", "intruder", "Hermes", "hermes ", "dreamer2", "approver "} {
		r := req("memory.get", TierInternal, Read)
		r.Actor = a
		if d := Decide(r, State{}, dreamerCfg(), now); d.Outcome != Deny || d.RuleID != "actor.unknown" {
			t.Errorf("actor %q: %+v", a, d)
		}
	}
}

func TestHermesIsUnchanged(t *testing.T) {
	r := req("memory.propose_update", TierInternal, Write)
	r.Actor = ActorHermes
	if d := Decide(r, State{}, baseCfg(), now); d.Outcome != Allow {
		t.Fatalf("%+v", d)
	}
	if d := Decide(r, State{Tainted: true, TaintSource: "web"}, baseCfg(), now); d.Outcome != NeedApproval || d.RuleID != "taint.write" {
		t.Fatalf("taint must still escalate hermes: %+v", d)
	}
}

func TestDreamerProposalCap(t *testing.T) {
	cfg := dreamerCfg()
	r := dreq("memory.propose_update", Write)
	if d := Decide(r, State{Spent: map[string]int64{CapDreamerProposals: 4}}, cfg, now); d.Outcome != Allow {
		t.Fatalf("the fifth proposal is within a cap of five: %+v", d)
	}
	d := Decide(r, State{Spent: map[string]int64{CapDreamerProposals: 5}}, cfg, now)
	if d.Outcome != Deny || d.RuleID != "dreamer.cap.proposals" {
		t.Fatalf("the sixth must be refused: %+v", d)
	}
	cfg.Dreamer.ProposalsPerDay = 0 // missing reads as zero
	d = Decide(r, State{}, cfg, now)
	if d.Outcome != Deny || d.RuleID != "dreamer.cap.proposals" || !strings.Contains(d.Reason, "zero") {
		t.Fatalf("a missing cap is zero: %+v", d)
	}
	// reads are not proposals
	if d := Decide(dreq("memory.get", Read), State{Spent: map[string]int64{CapDreamerProposals: 99}}, dreamerCfg(), now); d.Outcome != Allow {
		t.Fatalf("a read must not draw on the proposal cap: %+v", d)
	}
}

func TestDreamerRateLimitIsItsOwn(t *testing.T) {
	cfg := dreamerCfg()
	key := CallKey(ActorDreamer, "memory.get")
	if d := Decide(dreq("memory.get", Read), State{Calls: map[string]int{key: 299}}, cfg, now); d.Outcome != Allow {
		t.Fatalf("%+v", d)
	}
	d := Decide(dreq("memory.get", Read), State{Calls: map[string]int{key: 300}}, cfg, now)
	if d.Outcome != Deny || d.RuleID != "ratelimit.tool" {
		t.Fatalf("%+v", d)
	}
	// Hermes' calls neither use up the dreamer's allowance nor are limited by it.
	st := State{Calls: map[string]int{"memory.get": 500}}
	if d := Decide(dreq("memory.get", Read), st, cfg, now); d.Outcome != Allow {
		t.Fatalf("hermes' count must not count against the dreamer: %+v", d)
	}
	h := req("memory.get", TierInternal, Read)
	h.Actor = ActorHermes
	if d := Decide(h, State{Calls: map[string]int{key: 500}}, cfg, now); d.Outcome != Allow {
		t.Fatalf("the dreamer's count must not count against hermes: %+v", d)
	}
	cfg.Dreamer.CallsPerHour = 0
	if d := Decide(dreq("memory.get", Read), State{}, cfg, now); d.Outcome != Deny || d.RuleID != "ratelimit.unset" {
		t.Fatalf("an unset limit is zero calls: %+v", d)
	}
}

func TestTaintDoesNotTurnTheDreamersProposalIntoASecondApproval(t *testing.T) {
	// Taint is the web-browsing session's state. The dreamer reads and proposes through tools that
	// bring in no untrusted content, and a proposal already waits for the owner's tap.
	d := Decide(dreq("memory.propose_update", Write), State{Tainted: true, TaintSource: "web.fetch"}, dreamerCfg(), now)
	if d.Outcome != Allow {
		t.Fatalf("%+v", d)
	}
}

func TestKillSwitchStopsTheDreamersWrites(t *testing.T) {
	d := Decide(dreq("memory.propose_update", Write), State{KillSwitch: true, KilledAt: now}, dreamerCfg(), now)
	if d.Outcome != Deny || d.RuleID != "killswitch.active" {
		t.Fatalf("%+v", d)
	}
}

func TestTheApproverMayOnlyApplyAMemoryLine(t *testing.T) {
	r := req("memory.apply", TierInternal, Write)
	r.Actor = ActorApprover
	if d := Decide(r, State{}, baseCfg(), now); d.Outcome != Allow {
		t.Fatalf("%+v", d)
	}
	for _, tool := range []string{"memory.get", "memory.propose_update", "memory.propose_remove", "health.get_summary", "capabilities", "swiggy.order", "memory.merge"} {
		r := req(tool, TierInternal, Write)
		r.Actor = ActorApprover
		if d := Decide(r, State{}, baseCfg(), now); d.Outcome != Deny || d.RuleID != "actor.tool_not_allowed" {
			t.Errorf("%s: %+v", tool, d)
		}
	}
	// A tap does not outrank /stop.
	if d := Decide(r, State{KillSwitch: true, KilledAt: now}, baseCfg(), now); d.Outcome != Deny {
		t.Fatalf("%+v", d)
	}
}
