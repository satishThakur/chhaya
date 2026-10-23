package policy

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)

func baseCfg() Config {
	return Config{
		Caps:               map[string]Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
		DefaultRatePerHour: 60,
		ApprovalTimeout:    5 * time.Minute,
		BranchPrefix:       "agent/",
	}
}

func req(tool string, tier Tier, kind Kind) Request {
	return Request{Tool: tool, Tier: tier, Kind: kind, Known: true, ArgsOK: true, ValidatorOK: true}
}

func TestDecideEvaluationOrder(t *testing.T) {
	killed := State{KillSwitch: true, KilledAt: now.Add(-20 * time.Minute)}
	tainted := State{Tainted: true, TaintedAt: now.Add(-time.Minute), TaintSource: "web.fetch"}

	tests := []struct {
		name string
		req  Request
		st   State
		want Outcome
		rule string
	}{
		{"unknown tool denies",
			Request{Tool: "nope", Known: false}, State{}, Deny, "tool.unknown"},

		{"unknown tool denies even when the kill switch would also deny",
			Request{Tool: "nope", Known: false}, killed, Deny, "tool.unknown"},

		{"unparseable arguments deny before the kill switch is even consulted",
			Request{Tool: "memory.propose_update", Known: true, ArgsOK: false, Kind: Write}, killed, Deny, "args.unparseable"},

		{"kill switch denies a tier-0 write",
			req("memory.propose_update", TierInternal, Write), killed, Deny, "killswitch.active"},

		{"kill switch does not block a read",
			req("capabilities", TierInternal, Read), killed, Allow, "tier0.allow"},

		{"tier 3 denies even when nothing else is wrong",
			req("bank.transfer", TierIrreversible, Write), State{}, Deny, "tier3.not_exposed"},

		{"tier 3 denies before taint can soften it to approval",
			req("bank.transfer", TierIrreversible, Write), tainted, Deny, "tier3.not_exposed"},

		{"taint escalates a tier-0 write to approval",
			req("memory.propose_update", TierInternal, Write), tainted, NeedApproval, "taint.write"},

		{"taint does not touch reads",
			req("memory.get", TierInternal, Read), tainted, Allow, "tier0.allow"},

		{"tier 0 allows",
			req("ping", TierInternal, Read), State{}, Allow, "tier0.allow"},

		{"tier 1 allows when validators pass",
			req("code.run_task", TierReversible, Write), State{}, Allow, "tier1.validated"},

		{"tier 2 needs approval",
			func() Request {
				r := req("food.place_order", TierMoney, Write)
				r.CapKey = "food"
				r.AmountMinor = 40000
				return r
			}(),
			State{}, NeedApproval, "tier2.approval"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.req, tc.st, baseCfg(), now)
			if got.Outcome != tc.want {
				t.Errorf("outcome = %v, want %v (rule %q)", got.Outcome, tc.want, got.RuleID)
			}
			if got.RuleID != tc.rule {
				t.Errorf("rule = %q, want %q", got.RuleID, tc.rule)
			}
			if got.Reason == "" {
				t.Error("every decision must carry a human-readable reason")
			}
		})
	}
}

func TestDecideTier1FailedValidatorDenies(t *testing.T) {
	r := req("code.run_task", TierReversible, Write)
	r.ValidatorOK = false
	r.ValidatorErr = "branch must start with agent/"

	got := Decide(r, State{}, baseCfg(), now)
	if got.Outcome != Deny || got.RuleID != "validator.failed" {
		t.Fatalf("got %v/%q, want Deny/validator.failed", got.Outcome, got.RuleID)
	}
	if !strings.Contains(got.Reason, "agent/") {
		t.Error("the validator's own message should reach the agent so it can correct itself")
	}
}

func TestDecideCaps(t *testing.T) {
	order := func(amount int64) Request {
		r := req("food.place_order", TierMoney, Write)
		r.CapKey, r.AmountMinor = "food", amount
		return r
	}

	t.Run("over per-order cap denies", func(t *testing.T) {
		got := Decide(order(90000), State{}, baseCfg(), now)
		if got.Outcome != Deny || got.RuleID != "cap.food.per_order" {
			t.Fatalf("got %v/%q", got.Outcome, got.RuleID)
		}
	})

	t.Run("over daily cap denies and states the remainder", func(t *testing.T) {
		st := State{Spent: map[string]int64{"food": 130000}}
		got := Decide(order(40000), st, baseCfg(), now)
		if got.Outcome != Deny || got.RuleID != "cap.food.daily" {
			t.Fatalf("got %v/%q", got.Outcome, got.RuleID)
		}
		if !strings.Contains(got.Reason, "200") {
			t.Errorf("reason should state the ₹200 remaining, got %q", got.Reason)
		}
	})

	t.Run("a cap key with no configured cap denies", func(t *testing.T) {
		r := order(100)
		r.CapKey = "groceries"
		got := Decide(r, State{}, baseCfg(), now)
		if got.Outcome != Deny || got.RuleID != "cap.unknown" {
			t.Fatalf("an unconfigured cap must be zero, not infinity; got %v/%q", got.Outcome, got.RuleID)
		}
	})
}

func TestDecideRateLimit(t *testing.T) {
	t.Run("at the limit denies", func(t *testing.T) {
		st := State{Calls: map[string]int{"ping": 60}}
		got := Decide(req("ping", TierInternal, Read), st, baseCfg(), now)
		if got.Outcome != Deny || got.RuleID != "ratelimit.tool" {
			t.Fatalf("got %v/%q", got.Outcome, got.RuleID)
		}
	})

	t.Run("a config with no rate limit denies rather than allowing everything", func(t *testing.T) {
		cfg := baseCfg()
		cfg.DefaultRatePerHour = 0
		got := Decide(req("ping", TierInternal, Read), State{}, cfg, now)
		if got.Outcome != Deny || got.RuleID != "ratelimit.unset" {
			t.Fatalf("a missing limit must be zero, not infinity; got %v/%q", got.Outcome, got.RuleID)
		}
	})
}

// allInputs enumerates a cross-product wide enough that a rule added in the
// wrong position is caught by a property rather than by someone noticing.
func allInputs() []Request {
	var out []Request
	for _, tier := range []Tier{TierInternal, TierReversible, TierMoney, TierIrreversible} {
		for _, kind := range []Kind{Read, Write} {
			for _, vok := range []bool{true, false} {
				out = append(out, Request{
					Tool: "t", Tier: tier, Kind: kind, Known: true, ArgsOK: true, ValidatorOK: vok,
				})
			}
		}
	}
	return out
}

func TestNoWriteIsAllowedWhileKillSwitchIsOn(t *testing.T) {
	st := State{KillSwitch: true, KilledAt: now.Add(-time.Hour)}
	for _, r := range allInputs() {
		if r.Kind != Write {
			continue
		}
		if got := Decide(r, st, baseCfg(), now); got.Outcome == Allow {
			t.Fatalf("tier %d write allowed while stopped (rule %q)", r.Tier, got.RuleID)
		}
	}
}

func TestNoWriteIsAllowedWhileTainted(t *testing.T) {
	st := State{Tainted: true, TaintedAt: now, TaintSource: "web"}
	for _, r := range allInputs() {
		if r.Kind != Write {
			continue
		}
		if got := Decide(r, st, baseCfg(), now); got.Outcome == Allow {
			t.Fatalf("tier %d write allowed while tainted (rule %q)", r.Tier, got.RuleID)
		}
	}
}

func TestTierNeverComesFromArgs(t *testing.T) {
	r := req("memory.propose_update", TierInternal, Write)
	r.Args = map[string]any{"tier": 3, "Tier": 3, "policy": map[string]any{"tier": 3}}

	got := Decide(r, State{}, baseCfg(), now)
	if got.Tier != TierInternal {
		t.Fatalf("args influenced the tier: got %d", got.Tier)
	}
	if got.Outcome != Allow {
		t.Fatalf("args changed the outcome: %v/%q", got.Outcome, got.RuleID)
	}
}

func TestZeroValuesFailClosed(t *testing.T) {
	// A Request nobody filled in must not be allowed.
	if got := Decide(Request{}, State{}, Config{}, now); got.Outcome != Deny {
		t.Fatalf("the zero Request was not denied: %v/%q", got.Outcome, got.RuleID)
	}
	var o Outcome
	if o != Deny {
		t.Fatal("the zero Outcome must be Deny")
	}
	var k Kind
	if k != Write {
		t.Fatal("the zero Kind must be Write")
	}
}
