# Policy Gateway (M0b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the policy gateway skeleton — every safety mechanism real and exercised, with nothing valuable behind it yet — so that a blocked write is provably logged before code, money or health data go on the other side of the boundary.

**Architecture:** Two Go binaries. `chhayad` is a standalone HTTPS+mTLS service that is the only MCP server Hermes can see; every tool call enters through one exported function, `Gateway.Invoke`, which always runs `policy.Decide` → dispatch-or-refuse → `audit.Append`. `chhayactl` is a separate CLI so that verifying the audit log never runs inside the process that writes it. The policy engine is pure — no I/O, no ambient clock, no network — so every safety property is a table test with no mocks.

**Tech Stack:** Go 1.25+, `github.com/modelcontextprotocol/go-sdk` v1.8.0, `gopkg.in/yaml.v3`, standard-library `crypto/tls` and `net/http`. No Telegram library — the Bot API is four HTTP calls and a security component should not take a dependency for that.

**Spec:** [`docs/superpowers/specs/2026-10-21-policy-gateway-m0b-design.md`](../specs/2026-10-21-policy-gateway-m0b-design.md)

## Global Constraints

Every task's requirements implicitly include this section.

- **Module path** `github.com/satishthakur/chhaya`.
- **`go 1.25.0` in `go.mod`.** The spec says `go 1.24`; that is wrong and this plan corrects it. Verified: `go list -m` reports `github.com/modelcontextprotocol/go-sdk v1.8.0 go>=1.25.0`. The installed toolchain is go1.24.3, which builds this fine by auto-downloading go1.25.0 on first build — expect a one-time toolchain download, not an error.
- **MCP SDK pinned at `v1.8.0`**, upgraded deliberately as with the Hermes pin.
- **Standalone HTTPS+mTLS service, never a Hermes subprocess.** `tls.RequireAndVerifyClientCert`, `MinVersion: tls.VersionTLS13`.
- **`/stop` and `/resume` are on the approver bot, not Hermes' bot.**
- **Code decides policy; `config/policy.yaml` supplies values only.** No rule lives in YAML.
- **A missing or unparseable value resolves to the most restrictive interpretation, never the most permissive.** A missing cap is zero, not infinity. A typo can only tighten the boundary.
- **Fail closed.** If the gateway cannot determine a tier, read its own policy state, or reach the approver, it refuses the write.
- **Refuse to start if the approver chat is unreachable.**
- **Hermes pinned by digest `sha256:1d33f6087fab79d580ffaec34ceed825e11d7bb8e803206be75a56d027c62538` (v0.22.3).**
- **Secrets come from env only. Never log them.** Redaction happens *before* hashing, so a secret never enters the audit chain.
- **Go for everything we write.**
- **The audit log is tamper-evident** — hash-chained, append-only.
- **Changes to `internal/policy` need tests that prove a blocked case stays blocked.** Write the failing test first.
- **Assert on behaviour or structure, never on wording.** A spike check that string-matched a Hermes comment went red when the comment was rephrased with no change in meaning. A test that fails on rewording trains you to ignore it, and a safety test you have learned to ignore is worse than no test.
- **Never edit files under `memory/` except through `memory.propose_update` flows.**
- `make test` must pass before any task is called done. Run it; don't predict it.

## Two deviations from the spec, and why

Both are recorded here so the executor does not treat them as drift.

1. **`policy.Request` gains three fields the spec's struct does not list:** `Known bool`, `ValidatorOK bool`, and `CapKey string` / `AmountMinor int64`. The spec's §4.2 evaluation order includes "unknown tool → Deny", "tier 1 → Allow if validators pass" and "cap exceeded → Deny", but its §4.1 `Request` carries no way to express any of them. Without these fields those three rules would have to live outside `Decide`, and the spec is explicit that the evaluation *order* is the security property and is exhaustively tested in `internal/policy`. All three zero-value to the restrictive reading: `Known=false` denies, `ValidatorOK=false` denies.

2. **A new package, `internal/state`.** Spec §3.2 lists no owner for persisted kill-switch and taint state, but §6 step 6 requires `/stop` to survive a restart, and §3.2 also requires `internal/policy` to be pure with no I/O. Something has to own the file. `internal/state` is that something: it reads and writes the state file and hands `internal/policy` a plain `State` value.

## File Structure

```
go.mod  go.sum  Makefile
config/policy.yaml              values only — caps, allowlists, timeouts
cmd/chhayad/main.go             wiring and the startup sequence, no logic
cmd/chhayactl/main.go           audit verify · policy explain · stop
internal/policy/
  policy.go                     Tier/Kind/Outcome/Request/State/Decision types
  decide.go                     Decide() — the evaluation order, pure
  decide_test.go                table tests over every rule and interaction
internal/config/
  config.go                     policy.yaml load, strict decode, fail-closed validate
  config_test.go
internal/state/
  state.go                      persisted kill switch + taint + counters
  state_test.go
internal/audit/
  record.go                     Record type and canonical hashing
  redact.go                     redact-before-hash, default-deny on secret-ish keys
  writer.go                     append-only writer, chain tail
  verify.go                     independent verifier
  audit_test.go
internal/approval/
  telegram.go                   Telegram interface + net/http implementation
  bot.go                        long poll, /stop /resume /clear /status, cards
  bot_test.go                   fake transport
internal/mcpsrv/
  registry.go                   unexported registry; registration is data
  gateway.go                    Gateway.Invoke — the one door
  server.go                     MCP server + mTLS http.Server
  mcpsrv_test.go                bypass invariant, mTLS negative control
internal/tools/
  tools.go                      ping · memory.get · memory.propose_update · capabilities
  tools_test.go
deploy/
  docker-compose.yml
  gen-certs.sh
test/e2e/
  e2e_test.go                   the exit check, end to end
```

---

### Task 1: `internal/policy` — the evaluation order

The heart of the system and the only part with no dependencies. Everything else exists to feed it inputs and record its outputs.

**Files:**
- Create: `go.mod`, `Makefile`
- Create: `internal/policy/policy.go`
- Create: `internal/policy/decide.go`
- Test: `internal/policy/decide_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `policy.Tier`, `policy.Kind`, `policy.Outcome`, `policy.Request`, `policy.State`, `policy.Config`, `policy.Cap`, `policy.Decision`, and `func Decide(req Request, st State, cfg Config, now time.Time) Decision`.

- [ ] **Step 1: Initialise the module and a Makefile**

```bash
cd /home/satishthakur/codebase/chhaya
go mod init github.com/satishthakur/chhaya
go mod edit -go=1.25.0
```

```makefile
# Makefile
.PHONY: test build evals

test:
	go test ./...

build:
	go build -o bin/chhayad ./cmd/chhayad
	go build -o bin/chhayactl ./cmd/chhayactl

evals:
	go test ./internal/policy/... -v
```

`evals` points at the policy package on purpose: spec §8 says those tests *are* the L1 safety evals, not a separate artefact. Run the whole package, with no `-run` filter — `-run 'TestDecide'` is an unanchored substring match and would silently skip the four property tests whose names do not contain it, which are exactly the invariants §8 names.

- [ ] **Step 2: Write the types**

Create `internal/policy/policy.go`:

```go
// Package policy decides whether a tool call is allowed. It is pure: no I/O,
// no ambient clock, no network. The clock is injected because caps need it.
// Every safety property is therefore a table test with no mocks.
package policy

import "time"

// Tier numbering matches docs/design.md and appears verbatim in audit records.
type Tier int

const (
	TierInternal     Tier = 0 // internal: memory, status, no external effect
	TierReversible   Tier = 1 // reversible: a branch, a draft, an undoable write
	TierMoney        Tier = 2 // money, or data leaving the box
	TierIrreversible Tier = 3 // never exposed; registering one is a bug
)

// Kind zero-values to Write. A call whose kind was never set is treated as the
// more dangerous of the two, so forgetting costs approvals, never safety.
type Kind int

const (
	Write Kind = iota
	Read
)

// Outcome zero-values to Deny for the same reason.
type Outcome int

const (
	Deny Outcome = iota
	Allow
	NeedApproval
)

func (o Outcome) String() string {
	switch o {
	case Allow:
		return "allow"
	case NeedApproval:
		return "needs_approval"
	default:
		return "deny"
	}
}

func (k Kind) String() string {
	if k == Read {
		return "read"
	}
	return "write"
}

// Request is what the gateway asks about. Every field except Args comes from
// the tool's registration, never from the caller — a caller cannot propose its
// own tier, its own kind, or its own existence.
type Request struct {
	Tool string
	Args map[string]any
	Kind Kind
	Tier Tier

	// Known reports whether the registry recognised Tool. False denies.
	Known bool

	// ValidatorOK reports whether the tool's own validators passed. Consulted
	// only for tier 1. False denies.
	ValidatorOK  bool
	ValidatorErr string

	// CapKey names the spending cap this call draws on ("food"). Empty means
	// no spend cap applies. AmountMinor is in minor units (paise).
	CapKey      string
	AmountMinor int64
}

// State is the gateway's mutable world, read from internal/state.
type State struct {
	Tainted     bool
	TaintedAt   time.Time
	TaintSource string

	KillSwitch bool
	KilledAt   time.Time

	Spent map[string]int64 // minor units, per cap key, for the current day
	Calls map[string]int   // per-tool call count in the current hour
}

// Cap is a spending limit in minor units.
type Cap struct {
	PerOrderMinor int64
	PerDayMinor   int64
}

// Config holds values supplied by config/policy.yaml. It holds no rules.
type Config struct {
	Caps               map[string]Cap
	DefaultRatePerHour int
	ApprovalTimeout    time.Duration
	BranchPrefix       string
	Repos              []string
}

// Decision is the whole output. RuleID appears in both the approval card and
// the audit record, so a refusal can always be traced to the rule that caused it.
type Decision struct {
	Outcome Outcome
	Tier    Tier
	RuleID  string
	Reason  string
	Redact  []string
}
```

- [ ] **Step 3: Write the failing tests for the evaluation order**

Create `internal/policy/decide_test.go`. This is the file that matters most in the repo; write it before `decide.go` exists.

```go
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
	return Request{Tool: tool, Tier: tier, Kind: kind, Known: true, ValidatorOK: true}
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
			func() Request { r := req("food.place_order", TierMoney, Write); r.CapKey = "food"; r.AmountMinor = 40000; return r }(),
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
```

- [ ] **Step 4: Write the invariant tests**

Append to `internal/policy/decide_test.go`. These are the three properties spec §8 names, asserted directly rather than inferred from the table.

```go
// allInputs enumerates a cross-product wide enough that a rule added in the
// wrong position is caught by a property rather than by someone noticing.
func allInputs() []Request {
	var out []Request
	for _, tier := range []Tier{TierInternal, TierReversible, TierMoney, TierIrreversible} {
		for _, kind := range []Kind{Read, Write} {
			for _, vok := range []bool{true, false} {
				out = append(out, Request{
					Tool: "t", Tier: tier, Kind: kind, Known: true, ValidatorOK: vok,
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
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test ./internal/policy/...`
Expected: FAIL — `undefined: Decide`.

- [ ] **Step 6: Implement `Decide`**

Create `internal/policy/decide.go`:

```go
package policy

import (
	"fmt"
	"time"
)

// Decide applies the evaluation order from the M0b spec, §4.2. The order is
// the security property: each rule can only ever make the answer stricter than
// the rules below it, so reading top to bottom tells you what cannot happen.
func Decide(req Request, st State, cfg Config, now time.Time) Decision {
	d := Decision{Tier: req.Tier}

	// 1. Unknown tool. First, so that nothing below can be reached by a name
	//    the registry never approved.
	if !req.Known {
		return deny(d, "tool.unknown", fmt.Sprintf("blocked: %q is not a registered tool", req.Tool))
	}

	// 2. Kill switch, writes only. Reads stay open deliberately: a gateway that
	//    cannot answer capabilities() while stopped cannot explain why it is stopped.
	if st.KillSwitch && req.Kind == Write {
		return deny(d, "killswitch.active",
			fmt.Sprintf("blocked: kill switch active since %s; /resume on the approver bot to lift it",
				st.KilledAt.UTC().Format(time.RFC3339)))
	}

	// 3. Tier 3 is never exposed. Registering one is a bug; this is the backstop.
	if req.Tier == TierIrreversible {
		return deny(d, "tier3.not_exposed", "blocked: irreversible actions are not exposed through this gateway")
	}

	// 4. Taint escalates any write to approval, whatever its tier.
	if st.Tainted && req.Kind == Write {
		d.Outcome = NeedApproval
		d.RuleID = "taint.write"
		d.Reason = fmt.Sprintf("approval required: untrusted content from %s entered the session at %s; /clear on the approver bot to reset",
			st.TaintSource, st.TaintedAt.UTC().Format(time.RFC3339))
		return withCaps(d, req, st, cfg)
	}

	// 5. Tier resolution.
	switch req.Tier {
	case TierInternal:
		d.Outcome, d.RuleID, d.Reason = Allow, "tier0.allow", "allowed: internal action"
	case TierReversible:
		if !req.ValidatorOK {
			msg := req.ValidatorErr
			if msg == "" {
				msg = "validator rejected the arguments"
			}
			return deny(d, "validator.failed", "blocked: "+msg)
		}
		d.Outcome, d.RuleID, d.Reason = Allow, "tier1.validated", "allowed: reversible action, validators passed"
	case TierMoney:
		d.Outcome, d.RuleID, d.Reason = NeedApproval, "tier2.approval", "approval required: this spends money or sends data outside"
	default:
		// Unreachable given the check above, but an unrecognised tier must not
		// fall through to Allow.
		return deny(d, "tier.unknown", fmt.Sprintf("blocked: tier %d is not a tier this gateway knows", req.Tier))
	}

	// 6. Caps and rate limits. They can only tighten what step 5 decided.
	return withCaps(d, req, st, cfg)
}

func withCaps(d Decision, req Request, st State, cfg Config) Decision {
	if req.CapKey != "" {
		cap, ok := cfg.Caps[req.CapKey]
		if !ok {
			return deny(d, "cap.unknown",
				fmt.Sprintf("blocked: no spending cap is configured for %q, which reads as zero", req.CapKey))
		}
		if req.AmountMinor > cap.PerOrderMinor {
			return deny(d, "cap."+req.CapKey+".per_order",
				fmt.Sprintf("blocked: %s is over the per-order cap of %s", rupees(req.AmountMinor), rupees(cap.PerOrderMinor)))
		}
		if st.Spent[req.CapKey]+req.AmountMinor > cap.PerDayMinor {
			left := cap.PerDayMinor - st.Spent[req.CapKey]
			if left < 0 {
				left = 0
			}
			return deny(d, "cap."+req.CapKey+".daily",
				fmt.Sprintf("blocked: over the daily cap; %s left today", rupees(left)))
		}
	}

	if cfg.DefaultRatePerHour <= 0 {
		return deny(d, "ratelimit.unset",
			"blocked: no rate limit is configured, which reads as zero calls per hour")
	}
	if st.Calls[req.Tool] >= cfg.DefaultRatePerHour {
		return deny(d, "ratelimit.tool",
			fmt.Sprintf("blocked: %s has been called %d times this hour, the limit is %d",
				req.Tool, st.Calls[req.Tool], cfg.DefaultRatePerHour))
	}

	return d
}

func deny(d Decision, rule, reason string) Decision {
	d.Outcome, d.RuleID, d.Reason = Deny, rule, reason
	return d
}

// rupees renders minor units for a human reading an approval card.
func rupees(minor int64) string {
	if minor%100 == 0 {
		return fmt.Sprintf("₹%d", minor/100)
	}
	return fmt.Sprintf("₹%d.%02d", minor/100, minor%100)
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/policy/... -v`
Expected: PASS, all cases.

If `TestDecideCaps/over daily cap denies and states the remainder` fails on the `"200"` substring, check `rupees`: 150000 − 130000 = 20000 minor = ₹200.

- [ ] **Step 8: Commit**

```bash
git add go.mod Makefile internal/policy/
git commit -m "feat(policy): evaluation order, pure and table-tested

The order is the security property, so it is one function read top to
bottom, with each rule only able to tighten what follows. Three invariants
are asserted directly rather than inferred from the table: no write is
allowed while stopped, no write is allowed while tainted, and args never
influence the tier. Zero values resolve to Deny and Write so that a field
nobody filled in costs approvals rather than safety."
```

---

### Task 2: `internal/config` — values only, fail-closed

**Files:**
- Create: `config/policy.yaml`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `policy.Config`, `policy.Cap` from Task 1.
- Produces: `func Load(path string) (policy.Config, string, error)` returning the config, its **policy hash** (hex sha256 of the raw file bytes, used by the startup card in Task 7), and an error.

- [ ] **Step 1: Add the dependency and write the config file**

```bash
go get gopkg.in/yaml.v3@v3.0.1
```

Create `config/policy.yaml`:

```yaml
# Values only. Every rule lives in internal/policy. If you are tempted to add
# a condition here, the condition belongs in Go and this file holds its number.
#
# A missing or unparseable value resolves to the most restrictive reading:
# a missing cap is zero, not infinity. A typo can only tighten the boundary.

caps:
  food:
    per_order_inr: 800
    per_day_inr: 1500
  rate_limits:
    default_per_hour: 60

allowlists:
  repos: []          # filled at M2; empty means no repo is writable
  branch_prefix: "agent/"

approvals:
  timeout_seconds: 300
```

- [ ] **Step 2: Write the failing tests**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const good = `
caps:
  food: {per_order_inr: 800, per_day_inr: 1500}
  rate_limits: {default_per_hour: 60}
allowlists:
  repos: []
  branch_prefix: "agent/"
approvals:
  timeout_seconds: 300
`

func TestLoadConvertsRupeesToMinorUnits(t *testing.T) {
	cfg, _, err := Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caps["food"].PerOrderMinor != 80000 {
		t.Errorf("per-order = %d, want 80000 paise", cfg.Caps["food"].PerOrderMinor)
	}
	if cfg.Caps["food"].PerDayMinor != 150000 {
		t.Errorf("per-day = %d, want 150000 paise", cfg.Caps["food"].PerDayMinor)
	}
	if cfg.ApprovalTimeout.Seconds() != 300 {
		t.Errorf("timeout = %v", cfg.ApprovalTimeout)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, _, err := Load(write(t, good+"\nbypass_everything: true\n"))
	if err == nil {
		t.Fatal("an unknown key must be rejected, not ignored")
	}
}

func TestLoadRejectsMissingSections(t *testing.T) {
	cases := map[string]string{
		"no caps":        "allowlists: {repos: [], branch_prefix: \"agent/\"}\napprovals: {timeout_seconds: 300}\n",
		"no rate limit":  "caps:\n  food: {per_order_inr: 800, per_day_inr: 1500}\nallowlists: {repos: [], branch_prefix: \"agent/\"}\napprovals: {timeout_seconds: 300}\n",
		"no timeout":     "caps:\n  food: {per_order_inr: 800, per_day_inr: 1500}\n  rate_limits: {default_per_hour: 60}\nallowlists: {repos: [], branch_prefix: \"agent/\"}\n",
		"no branch pfx":  "caps:\n  food: {per_order_inr: 800, per_day_inr: 1500}\n  rate_limits: {default_per_hour: 60}\nallowlists: {repos: []}\napprovals: {timeout_seconds: 300}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Load(write(t, body)); err == nil {
				t.Fatal("a missing required value must refuse to load, not default to permissive")
			}
		})
	}
}

func TestLoadRejectsNonPositiveValues(t *testing.T) {
	body := strings.Replace(good, "default_per_hour: 60", "default_per_hour: 0", 1)
	if _, _, err := Load(write(t, body)); err == nil {
		t.Fatal("a zero rate limit should be stated by omitting the tool, not by writing 0")
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("no config must mean no start")
	}
}

func TestPolicyHashChangesWithContent(t *testing.T) {
	_, h1, err := Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := Load(write(t, strings.Replace(good, "800", "900", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("the policy hash must change when the policy does — the startup card depends on it")
	}
	if len(h1) != 64 {
		t.Errorf("hash should be hex sha256, got %d chars", len(h1))
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/config/...`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 4: Implement**

Create `internal/config/config.go`:

```go
// Package config loads config/policy.yaml. The file supplies values; it never
// supplies rules. Anything absent or unreadable is an error, not a default,
// because the permissive reading of a typo is the one we must never take.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/satishthakur/chhaya/internal/policy"
	"gopkg.in/yaml.v3"
)

type file struct {
	Caps struct {
		Food struct {
			PerOrderINR *int64 `yaml:"per_order_inr"`
			PerDayINR   *int64 `yaml:"per_day_inr"`
		} `yaml:"food"`
		RateLimits struct {
			DefaultPerHour *int `yaml:"default_per_hour"`
		} `yaml:"rate_limits"`
	} `yaml:"caps"`
	Allowlists struct {
		Repos        []string `yaml:"repos"`
		BranchPrefix *string  `yaml:"branch_prefix"`
	} `yaml:"allowlists"`
	Approvals struct {
		TimeoutSeconds *int `yaml:"timeout_seconds"`
	} `yaml:"approvals"`
}

// Load reads and validates the policy file. It returns the config, the hex
// sha256 of the raw file bytes, and an error. The hash is what the startup
// card keys on, so it covers comments and formatting too: if the file changed
// at all, we would rather tell you.
func Load(path string) (policy.Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return policy.Config{}, "", fmt.Errorf("read policy file: %w", err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // an unknown key is a typo or a stale assumption; refuse both
	if err := dec.Decode(&f); err != nil {
		return policy.Config{}, hash, fmt.Errorf("parse %s: %w", path, err)
	}

	// Pointers everywhere above so that "absent" and "zero" are different
	// things. Absent is an error; zero is an error too, but a different one.
	perOrder, err := required(f.Caps.Food.PerOrderINR, "caps.food.per_order_inr")
	if err != nil {
		return policy.Config{}, hash, err
	}
	perDay, err := required(f.Caps.Food.PerDayINR, "caps.food.per_day_inr")
	if err != nil {
		return policy.Config{}, hash, err
	}
	rate, err := required(f.Caps.RateLimits.DefaultPerHour, "caps.rate_limits.default_per_hour")
	if err != nil {
		return policy.Config{}, hash, err
	}
	timeout, err := required(f.Approvals.TimeoutSeconds, "approvals.timeout_seconds")
	if err != nil {
		return policy.Config{}, hash, err
	}
	if f.Allowlists.BranchPrefix == nil {
		return policy.Config{}, hash, fmt.Errorf("allowlists.branch_prefix is required")
	}
	if perDay < perOrder {
		return policy.Config{}, hash, fmt.Errorf(
			"caps.food.per_day_inr (%d) is below per_order_inr (%d), which cannot be what you meant", perDay, perOrder)
	}

	return policy.Config{
		Caps: map[string]policy.Cap{
			"food": {PerOrderMinor: perOrder * 100, PerDayMinor: perDay * 100},
		},
		DefaultRatePerHour: rate,
		ApprovalTimeout:    time.Duration(timeout) * time.Second,
		BranchPrefix:       *f.Allowlists.BranchPrefix,
		// Empty means no repository is writable, which is the safe default.
		// Do not normalise to nil: yaml.v3 decodes an explicit `repos: []` to a
		// non-nil zero-length slice and only an omitted key to nil, and every
		// consumer tests len(), so the representation is not the invariant.
		Repos: f.Allowlists.Repos,
	}, hash, nil
}

type number interface{ ~int | ~int64 }

func required[T number](v *T, key string) (T, error) {
	var zero T
	if v == nil {
		return zero, fmt.Errorf("%s is required and has no safe default", key)
	}
	if *v <= 0 {
		return zero, fmt.Errorf("%s must be positive; to forbid something, remove the tool rather than setting %v", key, *v)
	}
	return *v, nil
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add config/policy.yaml internal/config/
git commit -m "feat(config): load policy.yaml strictly, fail closed on anything absent

Every value is a pointer during decode so that absent and zero are
distinguishable, and both are errors. KnownFields(true) makes an unknown
key a refusal rather than a silently ignored line. Load also returns the
hex sha256 of the raw bytes, which is what the startup card keys on."
```

---

### Task 3: `internal/audit` — the hash chain and redact-before-hash

**Files:**
- Create: `internal/audit/record.go`
- Create: `internal/audit/redact.go`
- Create: `internal/audit/writer.go`
- Create: `internal/audit/verify.go`
- Test: `internal/audit/audit_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (it takes plain strings and maps, deliberately — the log format must not move every time a policy type does).
- Produces:
  - `type Record struct{...}` with exported fields as listed below
  - `func Open(path string) (*Writer, error)` — opens append-only, verifies the tail, returns the writer
  - `func (w *Writer) Append(r Record) (Record, error)` — fills `Seq`, `Prev`, `Hash`, writes one line, fsyncs, returns the completed record
  - `func (w *Writer) Close() error`
  - `func Verify(path string) (n int, err error)` — independent of `Writer`; reads the file and walks the chain
  - `func Redact(args map[string]any, paths []string) map[string]any`

**On the canonical form.** The spec writes `hash = sha256(canonical_json(record_without_hash) ‖ prev)`. "Without hash" is implemented as *with `Hash` set to the empty string*, which avoids string surgery on the serialised line and is equally deterministic. `Args` is `json.RawMessage` so that the bytes written are the bytes hashed and the bytes read back — no float64 round-trip drift. `TS` is a string in RFC3339Nano for the same reason.

- [ ] **Step 1: Write the failing tests**

Create `internal/audit/audit_test.go`:

```go
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmpLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "audit.jsonl")
}

func rec(tool string) Record {
	return Record{
		Actor: "hermes", Tool: tool, Kind: "write", Tier: 0,
		Args:     mustRaw(map[string]any{"path": "taste/food"}),
		Decision: "deny", Rule: "killswitch.active", Reason: "kill switch active",
		Result: Result{Status: "refused"}, DurationMS: 3,
	}
}

func mustRaw(v any) json.RawMessage { return MarshalArgs(v) }

func TestAppendBuildsAChainThatVerifies(t *testing.T) {
	p := tmpLog(t)
	w, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ping", "memory.get", "memory.propose_update"} {
		if _, err := w.Append(rec(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := Verify(p)
	if err != nil {
		t.Fatalf("a log we just wrote must verify: %v", err)
	}
	if n != 3 {
		t.Errorf("verified %d records, want 3", n)
	}
}

func TestGenesisRecordHasNoPrev(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	got, err := w.Append(rec("ping"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	if got.Seq != 1 {
		t.Errorf("seq = %d, want 1", got.Seq)
	}
	if got.Prev != nil {
		t.Errorf("genesis prev = %v, want null", *got.Prev)
	}
	if !strings.HasPrefix(got.Hash, "sha256:") {
		t.Errorf("hash = %q, want a sha256: prefix", got.Hash)
	}
}

func TestMutatingAPastRecordFailsVerification(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	for i := 0; i < 3; i++ {
		w.Append(rec("ping"))
	}
	w.Close()

	lines := readLines(t, p)
	var r Record
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil {
		t.Fatal(err)
	}
	r.Decision = "allow" // the edit an attacker would actually want
	b, _ := json.Marshal(r)
	lines[1] = string(b)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	if _, err := Verify(p); err == nil {
		t.Fatal("flipping a past decision must break the chain")
	}
}

func TestTruncationFailsVerification(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	for i := 0; i < 4; i++ {
		w.Append(rec("ping"))
	}
	w.Close()

	// Drop a record from the middle — the case a naive "recompute the tail"
	// verifier would miss.
	lines := readLines(t, p)
	kept := append(append([]string{}, lines[:2]...), lines[3:]...)
	os.WriteFile(p, []byte(strings.Join(kept, "\n")+"\n"), 0o600)

	if _, err := Verify(p); err == nil {
		t.Fatal("a removed record must break the chain")
	}
}

func TestOpenRefusesABrokenChain(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	w.Append(rec("ping"))
	w.Append(rec("ping"))
	w.Close()

	lines := readLines(t, p)
	lines[0] = strings.Replace(lines[0], `"tool":"ping"`, `"tool":"pong"`, 1)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	if _, err := Open(p); err == nil {
		t.Fatal("a broken chain is a refusal to start, not a warning")
	}
}

func TestReopenContinuesTheChain(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	first, _ := w.Append(rec("ping"))
	w.Close()

	w2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w2.Append(rec("ping"))
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()

	if second.Seq != 2 {
		t.Errorf("seq after reopen = %d, want 2", second.Seq)
	}
	if second.Prev == nil || *second.Prev != first.Hash {
		t.Error("a restart must not start a new chain")
	}
	if _, err := Verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestRedactionHappensBeforeHashing(t *testing.T) {
	const secret = "8979753688:AAH-SUPER-SECRET-TOKEN"
	p := tmpLog(t)
	w, _ := Open(p)

	args := Redact(map[string]any{
		"path":  "taste/food",
		"diff":  secret,
		"inner": map[string]any{"authorization": "Bearer " + secret},
	}, []string{"diff"})

	r := rec("memory.propose_update")
	r.Args = mustRaw(args)
	got, err := w.Append(r)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the secret reached the log, and therefore the chain")
	}
	if !strings.Contains(string(got.Args), "<redacted:") {
		t.Errorf("args = %s, want a redaction marker", got.Args)
	}
	if !strings.Contains(string(got.Args), "taste/food") {
		t.Error("redaction should scrub the named path, not everything")
	}
}

// TestARedactedRecordStillVerifies closes the gap between "the secret is not in
// the file" and "the chain is still intact". The redaction marker contains the
// two characters json.Marshal would escape, so this is exactly the record most
// likely to break round-trip fidelity.
func TestARedactedRecordStillVerifies(t *testing.T) {
	p := tmpLog(t)
	w, _ := Open(p)
	r := rec("memory.propose_update")
	r.Args = mustRaw(Redact(map[string]any{
		"path": "taste/food", "diff": "secret diff body",
	}, []string{"diff"}))
	if _, err := w.Append(r); err != nil {
		t.Fatal(err)
	}
	w.Close()

	if _, err := Verify(p); err != nil {
		t.Fatalf("a redacted record broke the chain: %v", err)
	}
}

func TestRedactScrubsSecretishKeysWithoutBeingAsked(t *testing.T) {
	out := Redact(map[string]any{
		"bot_token":     "abc123",
		"nested":        map[string]any{"api_key": "def456", "path": "ok"},
		"harmless":      "visible",
	}, nil)

	b, _ := json.Marshal(out)
	s := string(b)
	for _, leaked := range []string{"abc123", "def456"} {
		if strings.Contains(s, leaked) {
			t.Errorf("%q survived default redaction: %s", leaked, s)
		}
	}
	if !strings.Contains(s, "visible") || !strings.Contains(s, "ok") {
		t.Errorf("default redaction was too greedy: %s", s)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/audit/...`
Expected: FAIL — `undefined: Open`, `undefined: Record`, `undefined: Redact`, `undefined: Verify`.

- [ ] **Step 3: Implement the record and its hash**

Create `internal/audit/record.go`:

```go
// Package audit writes a hash-chained, append-only log. Tamper-evidence is the
// point: anyone with the file can tell whether it has been edited, including us.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Record is one line of the log. Field order here is the serialised order, and
// the serialised bytes are what gets hashed, so reordering fields invalidates
// every existing chain. Don't, without a migration.
type Record struct {
	Seq        uint64          `json:"seq"`
	TS         string          `json:"ts"`   // RFC3339Nano, UTC; a string so it round-trips byte for byte
	Prev       *string         `json:"prev"` // null on the genesis record
	Actor      string          `json:"actor"`
	Tool       string          `json:"tool"`
	Kind       string          `json:"kind"`
	Tier       int             `json:"tier"`
	Args       json.RawMessage `json:"args"`
	Decision   string          `json:"decision"`
	Rule       string          `json:"rule"`
	Reason     string          `json:"reason"`
	Taint      Taint           `json:"taint"`
	Approval   *Approval       `json:"approval"`
	Result     Result          `json:"result"`
	DurationMS int64           `json:"duration_ms"`
	Severity   string          `json:"severity,omitempty"` // "critical" when the safety channel itself failed
	Hash       string          `json:"hash"`
}

type Taint struct {
	Tainted bool   `json:"tainted"`
	Source  string `json:"source,omitempty"`
}

type Approval struct {
	Answer    string `json:"answer"` // approved | denied_by_user | timeout
	LatencyMS int64  `json:"latency_ms"`
}

type Result struct {
	Status string `json:"status"` // ok | refused | error
	Error  string `json:"error,omitempty"`
}

// canonicalJSON is the ONE encoding mode this package uses — for hashing, for
// writing, and for verifying. It must be one mode: json.Marshal HTML-escapes
// `<`, `>` and `&`, so mixing it with an unescaped writer would hash different
// bytes than it writes, and the redaction marker `<redacted:NB>` contains two
// of those three characters.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil // Encode appends a newline
}

// MarshalArgs is the supported way to build Record.Args. Use it rather than
// json.Marshal so that the bytes written are the bytes hashed. It cannot fail
// usefully from a caller's point of view, so an unserialisable value becomes a
// sentinel rather than an error nobody can act on.
func MarshalArgs(v any) json.RawMessage {
	b, err := canonicalJSON(v)
	if err != nil {
		return json.RawMessage(`{"_":"args were not serialisable"}`)
	}
	return b
}

// computeHash implements the spec's sha256(canonical_json(record_without_hash) ‖ prev).
// "Without hash" is realised by zeroing the field rather than editing the
// serialised bytes: equally deterministic, and no string surgery to get wrong.
func (r Record) computeHash() (string, error) {
	c := r
	c.Hash = ""
	b, err := canonicalJSON(c)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(b)
	if r.Prev != nil {
		h.Write([]byte(*r.Prev))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
```

- [ ] **Step 4: Implement redaction**

Create `internal/audit/redact.go`:

```go
package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// secretish names whose values are never logged, whatever the policy said.
// CLAUDE.md requires the logger to redact by default; this is that default.
// It is deliberately a suffix/substring match rather than an exact list,
// because the failure mode we care about is a key nobody thought to name.
var secretish = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "authorization", "credential", "private_key"}

// Redact returns a copy of args with the named paths and any secret-ish key
// replaced by a size marker. Call it before handing args to Append: the whole
// point is that a secret never enters the hashed bytes, so there is no version
// of the log from which it could later be recovered.
//
// paths are dot-separated, e.g. "diff" or "body.content".
func Redact(args map[string]any, paths []string) map[string]any {
	out := deepCopyMap(args)
	for _, p := range paths {
		redactPath(out, strings.Split(p, "."))
	}
	redactSecretish(out)
	return out
}

func redactPath(m map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	k := parts[0]
	v, ok := m[k]
	if !ok {
		return
	}
	if len(parts) == 1 {
		m[k] = marker(v)
		return
	}
	if child, ok := v.(map[string]any); ok {
		redactPath(child, parts[1:])
	}
}

func redactSecretish(m map[string]any) {
	for k, v := range m {
		if child, ok := v.(map[string]any); ok {
			redactSecretish(child)
			continue
		}
		lk := strings.ToLower(k)
		for _, s := range secretish {
			if strings.Contains(lk, s) {
				m[k] = marker(v)
				break
			}
		}
	}
}

// marker records the size of what was removed, so the log still shows that
// something substantial was there without showing what.
func marker(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<redacted>"
	}
	return fmt.Sprintf("<redacted:%dB>", len(b))
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if child, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(child)
			continue
		}
		out[k] = v
	}
	return out
}
```

- [ ] **Step 5: Implement the writer and the verifier**

Create `internal/audit/writer.go`:

```go
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Writer appends to the log. One per process; Append is safe for concurrent use.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	lastSeq  uint64
	lastHash string
}

// Open opens the log for appending, verifying the existing chain first.
// A broken chain is a refusal, not a warning: if we cannot trust what the log
// already says, appending to it only makes the lie longer.
func Open(path string) (*Writer, error) {
	if _, err := os.Stat(path); err == nil {
		if _, err := Verify(path); err != nil {
			return nil, fmt.Errorf("refusing to append to a log that does not verify: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	seq, hash, err := tail(path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, lastSeq: seq, lastHash: hash}, nil
}

// Append completes r with its sequence, previous hash and own hash, writes it
// as one line, and fsyncs. It returns the completed record so the caller can
// log or assert on it.
func (w *Writer) Append(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	r.Seq = w.lastSeq + 1
	if r.TS == "" {
		r.TS = nowStamp()
	}
	if w.lastHash == "" {
		r.Prev = nil
	} else {
		prev := w.lastHash
		r.Prev = &prev
	}
	if r.Args == nil {
		r.Args = json.RawMessage("{}")
	}

	h, err := r.computeHash()
	if err != nil {
		return Record{}, err
	}
	r.Hash = h

	line, err := canonicalJSON(r) // same bytes the hash was taken over
	if err != nil {
		return Record{}, err
	}
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		return Record{}, err
	}
	// fsync on every record. The log is small and the thing it protects
	// against includes "the box lost power mid-incident".
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}

	w.lastSeq, w.lastHash = r.Seq, r.Hash
	return r, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func tail(path string) (uint64, string, error) {
	recs, err := readAll(path)
	if err != nil {
		return 0, "", err
	}
	if len(recs) == 0 {
		return 0, "", nil
	}
	last := recs[len(recs)-1]
	return last.Seq, last.Hash, nil
}
```

Create `internal/audit/verify.go`:

```go
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Verify walks the chain from the genesis record and reports how many records
// it checked. It shares no state with Writer on purpose — chhayactl runs it in
// a different process, so a compromised daemon cannot also be the auditor.
func Verify(path string) (int, error) {
	recs, err := readAll(path)
	if err != nil {
		return 0, err
	}

	var prev *string
	for i, r := range recs {
		want := uint64(i + 1)
		if r.Seq != want {
			return i, fmt.Errorf("record %d: seq is %d, expected %d — a record was removed or reordered", i+1, r.Seq, want)
		}
		if (prev == nil) != (r.Prev == nil) || (prev != nil && *prev != *r.Prev) {
			return i, fmt.Errorf("record %d (seq %d): prev does not match the previous record's hash", i+1, r.Seq)
		}
		got, err := r.computeHash()
		if err != nil {
			return i, err
		}
		if got != r.Hash {
			return i, fmt.Errorf("record %d (seq %d): content does not match its hash — this record was edited", i+1, r.Seq)
		}
		h := r.Hash
		prev = &h
	}
	return len(recs), nil
}

func readAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a diff can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("record %d is not valid JSON: %w", len(out)+1, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
```

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/audit/... -v`
Expected: PASS.

If `TestReopenContinuesTheChain` fails with a seq of 1, `tail` is not being consulted — check that `Open` passes its result into the `Writer`.

- [ ] **Step 7: Commit**

```bash
git add internal/audit/
git commit -m "feat(audit): hash-chained append-only log with redact-before-hash

Verify walks from genesis rather than recomputing the tail, so removing a
record from the middle is caught. Open refuses to append to a log that
does not verify: appending to a log we cannot trust only makes the lie
longer. Redact scrubs policy-named paths and, by default, any secret-ish
key — the failure we care about is the key nobody thought to name."
```

---

### Task 4: `internal/state` — kill switch and taint that survive a restart

Small, but it is what makes `/stop` mean something after a bounce.

**Files:**
- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Consumes: `policy.State` from Task 1.
- Produces:
  - `func Open(path string) (*Store, error)`
  - `func (s *Store) Snapshot(now time.Time) policy.State` — current state with day/hour counters rolled over
  - `func (s *Store) SetKillSwitch(on bool, now time.Time) error`
  - `func (s *Store) SetTaint(on bool, source string, now time.Time) error`
  - `func (s *Store) RecordCall(tool string, now time.Time) error`
  - `func (s *Store) RecordSpend(capKey string, minor int64, now time.Time) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/state/state_test.go`:

```go
package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)

func tmp(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

func TestKillSwitchSurvivesAReopen(t *testing.T) {
	p := tmp(t)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKillSwitch(true, t0); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Snapshot(t0.Add(time.Hour))
	if !got.KillSwitch {
		t.Fatal("a /stop must survive a restart; that is most of the point of persisting it")
	}
	if !got.KilledAt.Equal(t0) {
		t.Errorf("KilledAt = %v, want %v", got.KilledAt, t0)
	}
}

func TestMissingFileStartsClean(t *testing.T) {
	s, err := Open(tmp(t))
	if err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot(t0)
	if got.KillSwitch || got.Tainted {
		t.Fatal("a fresh box should not start stopped or tainted")
	}
}

func TestCorruptFileRefusesToOpen(t *testing.T) {
	p := tmp(t)
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("unreadable state must be a refusal: we cannot tell whether we are stopped")
	}
}

// TestFileThatParsesButHoldsNoStateRefusesToOpen covers the inputs that are
// valid JSON and therefore slip past a syntax check. `null` is the dangerous
// one: unmarshalled into a value target it succeeds and leaves the target
// zeroed, which reads as "not stopped".
func TestFileThatParsesButHoldsNoStateRefusesToOpen(t *testing.T) {
	for name, body := range map[string]string{
		"json null":        "null",
		"null with spaces": "  null \n",
		"empty object":     "{}",
		"wrong shape":      "[]",
		"empty file":       "",
		"unknown version":  `{"v":99,"kill_switch":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := tmp(t)
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(p); err == nil {
				t.Fatalf("%q was accepted; a file we cannot read as state must stop the gateway", body)
			}
		})
	}
}

func TestTaintRoundTrips(t *testing.T) {
	p := tmp(t)
	s, _ := Open(p)
	s.SetTaint(true, "web.fetch", t0)

	s2, _ := Open(p)
	got := s2.Snapshot(t0)
	if !got.Tainted || got.TaintSource != "web.fetch" {
		t.Fatalf("taint = %+v", got)
	}

	s2.SetTaint(false, "", t0.Add(time.Minute))
	if s2.Snapshot(t0.Add(time.Minute)).Tainted {
		t.Fatal("/clear should clear it")
	}
}

func TestSpendRollsOverAtMidnightUTC(t *testing.T) {
	s, _ := Open(tmp(t))
	s.RecordSpend("food", 40000, t0)

	if got := s.Snapshot(t0).Spent["food"]; got != 40000 {
		t.Fatalf("same day spend = %d, want 40000", got)
	}
	nextDay := t0.Add(24 * time.Hour)
	if got := s.Snapshot(nextDay).Spent["food"]; got != 0 {
		t.Fatalf("spend carried into the next day: %d", got)
	}
}

func TestCallCountRollsOverHourly(t *testing.T) {
	s, _ := Open(tmp(t))
	for i := 0; i < 5; i++ {
		s.RecordCall("ping", t0)
	}
	if got := s.Snapshot(t0).Calls["ping"]; got != 5 {
		t.Fatalf("calls = %d, want 5", got)
	}
	if got := s.Snapshot(t0.Add(time.Hour)).Calls["ping"]; got != 0 {
		t.Fatalf("calls carried into the next hour: %d", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/state/...`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Implement**

Create `internal/state/state.go`:

```go
// Package state owns the gateway's mutable world: the kill switch, the taint
// flag, and the per-day and per-hour counters. It exists because internal/policy
// must stay pure and something still has to survive a restart.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/satishthakur/chhaya/internal/policy"
)

// stateVersion is written by every flush and required by every Open. It is
// what makes "unreadable means stopped" true across the whole input space
// rather than only for syntax errors: a file holding `null` or `{}` parses
// without error but carries no state, and accepting either would silently
// turn the kill switch off.
const stateVersion = 1

type persisted struct {
	V           int              `json:"v"`
	KillSwitch  bool             `json:"kill_switch"`
	KilledAt    time.Time        `json:"killed_at"`
	Tainted     bool             `json:"tainted"`
	TaintedAt   time.Time        `json:"tainted_at"`
	TaintSource string           `json:"taint_source"`
	SpendDay    string           `json:"spend_day"` // YYYY-MM-DD, UTC
	Spent       map[string]int64 `json:"spent"`
	CallHour    string           `json:"call_hour"` // YYYY-MM-DDTHH, UTC
	Calls       map[string]int   `json:"calls"`
}

type Store struct {
	mu   sync.Mutex
	path string
	p    persisted
}

// Open reads the state file, or starts clean if there is none. An unreadable
// file is an error: if we cannot tell whether we are stopped, we are stopped.
func Open(path string) (*Store, error) {
	s := &Store{path: path, p: persisted{Spent: map[string]int64{}, Calls: map[string]int{}}}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	// Decode into a pointer, not a value: json.Unmarshal of the literal `null`
	// into a value target succeeds and leaves it untouched, so a file corrupted
	// down to four bytes would read as "not stopped".
	var p *persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("state file at %s is unreadable, so we cannot tell whether we are stopped: %w", path, err)
	}
	if p == nil {
		return nil, fmt.Errorf("state file at %s holds JSON null, not state; delete it deliberately if you mean to start clean", path)
	}
	if p.V != stateVersion {
		return nil, fmt.Errorf("state file at %s has version %d, want %d; refusing to guess what it means", path, p.V, stateVersion)
	}
	s.p = *p
	if s.p.Spent == nil {
		s.p.Spent = map[string]int64{}
	}
	if s.p.Calls == nil {
		s.p.Calls = map[string]int{}
	}
	return s, nil
}

// Snapshot returns the state as policy.Decide should see it at now, with
// expired counters already zeroed so the caller cannot forget to roll them.
func (s *Store) Snapshot(now time.Time) policy.State {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := policy.State{
		KillSwitch:  s.p.KillSwitch,
		KilledAt:    s.p.KilledAt,
		Tainted:     s.p.Tainted,
		TaintedAt:   s.p.TaintedAt,
		TaintSource: s.p.TaintSource,
		Spent:       map[string]int64{},
		Calls:       map[string]int{},
	}
	if s.p.SpendDay == day(now) {
		for k, v := range s.p.Spent {
			st.Spent[k] = v
		}
	}
	if s.p.CallHour == hour(now) {
		for k, v := range s.p.Calls {
			st.Calls[k] = v
		}
	}
	return st
}

func (s *Store) SetKillSwitch(on bool, now time.Time) error {
	return s.update(func(p *persisted) {
		p.KillSwitch = on
		if on {
			p.KilledAt = now
		}
	})
}

func (s *Store) SetTaint(on bool, source string, now time.Time) error {
	return s.update(func(p *persisted) {
		p.Tainted = on
		p.TaintSource = source
		if on {
			p.TaintedAt = now
		}
	})
}

func (s *Store) RecordCall(tool string, now time.Time) error {
	return s.update(func(p *persisted) {
		if p.CallHour != hour(now) {
			p.CallHour, p.Calls = hour(now), map[string]int{}
		}
		p.Calls[tool]++
	})
}

func (s *Store) RecordSpend(capKey string, minor int64, now time.Time) error {
	return s.update(func(p *persisted) {
		if p.SpendDay != day(now) {
			p.SpendDay, p.Spent = day(now), map[string]int64{}
		}
		p.Spent[capKey] += minor
	})
}

func (s *Store) update(f func(*persisted)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.p)
	s.p.V = stateVersion
	return s.flush()
}

// flush writes atomically and durably. Both halves matter: a half-written
// state file is an unreadable one, and an unreadable one stops the gateway.
// os.WriteFile would only reach the page cache, so a crash just after the
// rename could leave the target present with stale bytes — the exact failure
// the temp-file dance is supposed to prevent. Hence the explicit Sync.
func (s *Store) flush() error {
	b, err := json.Marshal(s.p)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp" // same directory, so Rename never crosses a filesystem
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func day(t time.Time) string  { return t.UTC().Format("2006-01-02") }
func hour(t time.Time) string { return t.UTC().Format("2006-01-02T15") }
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/state/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/state/
git commit -m "feat(state): persist the kill switch, taint and counters

Snapshot rolls expired day and hour counters itself so no caller can
forget to. An unreadable state file refuses to open: if we cannot tell
whether we are stopped, we are stopped. Writes go through a temp file and
a rename, because a half-written state file is an unreadable one."
```

---

### Task 5: `internal/mcpsrv` — the registry and the one door

The structural heart of the design. After this task, there is exactly one exported way to reach a tool handler.

> **Superseded in execution.** The reference code below was materially incomplete and the shipped implementation (commits `a02b178`, `a523068`, `95ada30`) differs in eight places. The largest: `dispatch` must set taint **before** invoking the handler, because an untrusted tool's *error string* is untrusted content too and the original ordering leaked it with the flag unset. Also changed: re-decide after an approval returns so `/stop` can interrupt a pending write; a deferred recover so a panicking handler cannot skip `audit.Append`; gateway-originated refusals recorded as `deny` with their own rule rather than `allow` with the policy's; `Redact`/`Schema` cloned on both ingress and egress; validators consulted whenever supplied; tier-2 registration rejected outright until caps are wired; a backstop timeout on the approver call. Read the code, not this section. The reasoning for each is in the SDD ledger.

**Files:**
- Create: `internal/mcpsrv/registry.go`
- Create: `internal/mcpsrv/gateway.go`
- Test: `internal/mcpsrv/gateway_test.go`

**Interfaces:**
- Consumes: `policy.Decide`, `policy.Config`, `policy.Request`, `policy.Tier`, `policy.Kind` (Task 1); `audit.Record`, `audit.Redact`, `audit.Writer` (Task 3); `state.Store` (Task 4).
- Produces:
  - `type Trust string` with `Trusted Trust = "trusted"` and `Untrusted Trust = "untrusted"`
  - `type Handler func(ctx context.Context, args map[string]any) (any, error)`
  - `type Validator func(args map[string]any) error`
  - `type Registration struct { Name, Description string; Tier policy.Tier; Kind policy.Kind; Trust Trust; Schema json.RawMessage; Validate Validator; CapKey string; Handle Handler }`
  - `type ToolCall struct { Actor, Name string; Args map[string]any }`
  - `type ToolResult struct { Value any; Trust Trust; IsError bool; Error string; RuleID string }`
  - `type Approver interface { Request(ctx context.Context, call ToolCall, d policy.Decision) (approved bool, latency time.Duration, err error) }`
  - `func New(cfg policy.Config, st *state.Store, w *audit.Writer, ap Approver, regs []Registration) (*Gateway, error)`
  - `func (g *Gateway) Invoke(ctx context.Context, call ToolCall) ToolResult`
  - `func (g *Gateway) Registrations() []Registration` — metadata only, no handlers; used by `capabilities` and `chhayactl policy explain`

- [ ] **Step 1: Write the failing tests, starting with the invariant**

Create `internal/mcpsrv/gateway_test.go`:

```go
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
```

- [ ] **Step 2: Write the behavioural tests**

Append to `internal/mcpsrv/gateway_test.go`:

```go
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
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/mcpsrv/...`
Expected: FAIL — `undefined: New`, `undefined: Gateway`, `undefined: Registration`.

- [ ] **Step 4: Implement the registry**

Create `internal/mcpsrv/registry.go`:

```go
package mcpsrv

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/satishthakur/chhaya/internal/policy"
)

// Trust is set by the gateway from a tool's registration. A handler cannot set
// it and a caller cannot ask for it. In M0b every tool is Trusted; the wiring
// exists so that the first untrusted source is covered by construction rather
// than by someone remembering.
type Trust string

const (
	Trusted   Trust = "trusted"
	Untrusted Trust = "untrusted"
)

type (
	Handler   func(ctx context.Context, args map[string]any) (any, error)
	Validator func(args map[string]any) error
)

// Registration is a tool described as data. Every field below is required
// except Validate and CapKey; New refuses anything incomplete, because
// CLAUDE.md says a tool with no tier is a bug, not a default.
type Registration struct {
	Name        string
	Description string // states the tool's limits, so the agent plans within them
	Tier        policy.Tier
	Kind        policy.Kind
	Trust       Trust
	Schema      json.RawMessage
	Validate    Validator // consulted for tier 1 only
	CapKey      string    // names the spending cap this tool draws on
	Redact      []string  // arg paths never written to the log
	Handle      Handler
}

// registry is unexported, and so is this type. Adding a path to a handler
// means editing this package — a reviewable event, not an easy accident.
type registry map[string]Registration

func buildRegistry(regs []Registration) (registry, error) {
	out := make(registry, len(regs))
	for _, r := range regs {
		switch {
		case r.Name == "":
			return nil, fmt.Errorf("a registration has no name")
		case r.Handle == nil:
			return nil, fmt.Errorf("%s: no handler", r.Name)
		case len(r.Schema) == 0:
			return nil, fmt.Errorf("%s: no input schema", r.Name)
		case r.Trust != Trusted && r.Trust != Untrusted:
			return nil, fmt.Errorf("%s: trust must be stated explicitly, not inferred", r.Name)
		case r.Tier == policy.TierIrreversible:
			return nil, fmt.Errorf("%s: tier 3 actions are never exposed; do not register one", r.Name)
		case r.Tier < policy.TierInternal || r.Tier > policy.TierMoney:
			return nil, fmt.Errorf("%s: tier %d is not a tier", r.Name, r.Tier)
		case r.Tier == policy.TierReversible && r.Validate == nil:
			return nil, fmt.Errorf("%s: a tier-1 tool must supply a validator", r.Name)
		}
		if _, dup := out[r.Name]; dup {
			return nil, fmt.Errorf("%s: registered twice", r.Name)
		}
		out[r.Name] = r
	}
	return out, nil
}
```

- [ ] **Step 5: Implement `Invoke`**

Create `internal/mcpsrv/gateway.go`:

```go
package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

var errUnreachable = errors.New("approver unreachable")

// Approver asks the human. It is an interface so that internal/mcpsrv does not
// depend on Telegram, and so the tests above need no network.
type Approver interface {
	Request(ctx context.Context, call ToolCall, d policy.Decision) (approved bool, latency time.Duration, err error)
}

type ToolCall struct {
	Actor string // "hermes"
	Name  string
	Args  map[string]any
}

type ToolResult struct {
	Value   any
	Trust   Trust
	IsError bool
	Error   string
	RuleID  string
}

type Gateway struct {
	cfg       policy.Config
	state     *state.Store
	audit     *audit.Writer
	auditPath string
	approver  Approver
	tools     registry
	now       func() time.Time
}

func New(cfg policy.Config, st *state.Store, w *audit.Writer, ap Approver, regs []Registration) (*Gateway, error) {
	reg, err := buildRegistry(regs)
	if err != nil {
		return nil, err
	}
	if ap == nil {
		return nil, errors.New("no approver: a gateway that cannot ask cannot approve")
	}
	return &Gateway{
		cfg: cfg, state: st, audit: w, auditPath: w.Path(),
		approver: ap, tools: reg, now: time.Now,
	}, nil
}

// Registrations returns tool metadata with handlers stripped. capabilities()
// and chhayactl both need to describe the tools; neither may call them.
func (g *Gateway) Registrations() []Registration {
	out := make([]Registration, 0, len(g.tools))
	for _, r := range g.tools {
		r.Handle, r.Validate = nil, nil
		out = append(out, r)
	}
	return out
}

// Invoke is the only exported path to a tool handler. It always runs, in order:
// policy.Decide → dispatch-or-refuse → audit.Append. There is no branch that
// skips the first or the last.
func (g *Gateway) Invoke(ctx context.Context, call ToolCall) ToolResult {
	start := g.now()
	reg, known := g.tools[call.Name]

	req := policy.Request{
		Tool:   call.Name,
		Args:   call.Args,
		Known:  known,
		Kind:   reg.Kind,
		Tier:   reg.Tier,
		CapKey: reg.CapKey,
	}
	if known && reg.Tier == policy.TierReversible {
		if err := reg.Validate(call.Args); err != nil {
			req.ValidatorErr = err.Error()
		} else {
			req.ValidatorOK = true
		}
	} else {
		req.ValidatorOK = true // only tier 1 consults it
	}

	st := g.state.Snapshot(start)
	d := policy.Decide(req, st, g.cfg, start)

	switch d.Outcome {
	case policy.Deny:
		return g.refuse(call, reg, st, d, start, "")

	case policy.NeedApproval:
		g.record(call, reg, st, d, "needs_approval", nil, audit.Result{Status: "pending"}, start, "")
		approved, latency, err := g.approver.Request(ctx, call, d)
		if err != nil {
			// The safety channel itself is down. Refuse loudly, and tell Hermes
			// in a way it can repeat to the user: the agent becomes the
			// out-of-band alarm precisely when the alarm is what is broken.
			ans := &audit.Approval{Answer: "channel_failed", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", ans,
				audit.Result{Status: "refused", Error: err.Error()}, start, "critical")
			return ToolResult{
				IsError: true, RuleID: "approval.unreachable",
				Error: "refused: the approval channel is unreachable, so this cannot be approved. " +
					"Tell the user their approver bot is not responding.",
			}
		}
		if !approved {
			ans := &audit.Approval{Answer: "denied_by_user", LatencyMS: latency.Milliseconds()}
			g.record(call, reg, st, d, "deny", ans, audit.Result{Status: "refused"}, start, "")
			return ToolResult{IsError: true, RuleID: d.RuleID, Error: "refused: you declined this"}
		}
		res := g.dispatch(ctx, reg, call, start)
		ans := &audit.Approval{Answer: "approved", LatencyMS: latency.Milliseconds()}
		g.record(call, reg, st, d, "allow", ans, resultOf(res), start, "")
		return res

	default: // policy.Allow
		res := g.dispatch(ctx, reg, call, start)
		g.record(call, reg, st, d, "allow", nil, resultOf(res), start, "")
		return res
	}
}

func (g *Gateway) dispatch(ctx context.Context, reg Registration, call ToolCall, start time.Time) ToolResult {
	if err := g.state.RecordCall(call.Name, start); err != nil {
		return ToolResult{IsError: true, RuleID: "state.unwritable",
			Error: "refused: the gateway cannot record this call, so it will not make it"}
	}
	v, err := reg.Handle(ctx, call.Args)
	if err != nil {
		return ToolResult{Trust: reg.Trust, IsError: true, Error: err.Error()}
	}
	// Trust is stamped here, from the registration. The handler had no say.
	if reg.Trust == Untrusted {
		if err := g.state.SetTaint(true, reg.Name, g.now()); err != nil {
			return ToolResult{IsError: true, RuleID: "state.unwritable",
				Error: "refused: untrusted content could not be marked as such, so it is not returned"}
		}
	}
	return ToolResult{Value: v, Trust: reg.Trust}
}

func (g *Gateway) refuse(call ToolCall, reg Registration, st policy.State, d policy.Decision, start time.Time, sev string) ToolResult {
	g.record(call, reg, st, d, "deny", nil, audit.Result{Status: "refused"}, start, sev)
	return ToolResult{IsError: true, RuleID: d.RuleID, Error: d.Reason}
}

func (g *Gateway) record(call ToolCall, reg Registration, st policy.State, d policy.Decision,
	decision string, ap *audit.Approval, res audit.Result, start time.Time, severity string) {

	// MarshalArgs, not json.Marshal: the audit package owns its encoding mode
	// so that the bytes written are the bytes hashed.
	args := audit.MarshalArgs(audit.Redact(call.Args, append(reg.Redact, d.Redact...)))

	r := audit.Record{
		Actor: orDefault(call.Actor, "unknown"), Tool: call.Name,
		Kind: reg.Kind.String(), Tier: int(d.Tier),
		Args: args, Decision: decision, Rule: d.RuleID, Reason: d.Reason,
		Taint:      audit.Taint{Tainted: st.Tainted, Source: st.TaintSource},
		Approval:   ap,
		Result:     res,
		DurationMS: g.now().Sub(start).Milliseconds(),
		Severity:   severity,
	}
	if _, err := g.audit.Append(r); err != nil {
		// An unwritable log is a fail-closed condition, but we are already on
		// the refusal path here; surfacing it to stderr is all we can do
		// without a log to write it to.
		fmt.Printf("CRITICAL: audit append failed: %v\n", err)
	}
}

func resultOf(r ToolResult) audit.Result {
	if r.IsError {
		return audit.Result{Status: "error", Error: r.Error}
	}
	return audit.Result{Status: "ok"}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
```

- [ ] **Step 6: Wire `auditPath` so tests can read the log**

In `New`, the gateway needs to know where the log lives. Change the `audit.Writer` to expose it rather than threading a second argument — add to `internal/audit/writer.go`:

```go
// Path reports the file this writer appends to. chhayactl and the tests read
// the log independently of the writer; this is how they find it.
func (w *Writer) Path() string { return w.f.Name() }
```

`New` already sets `auditPath: w.Path()` from this.

- [ ] **Step 7: Run to verify they pass**

Run: `go test ./internal/mcpsrv/... -v`
Expected: PASS.

If `TestRegistryIsUnreachableExceptThroughInvoke` fails naming `Registrations`, check that the allow-list in the test still matches reality — and if you added a new exported method that legitimately reads the registry, think hard before widening the allow-list. Widening it is the exact drift the test exists to catch.

- [ ] **Step 8: Commit**

```bash
git add internal/mcpsrv/ internal/audit/writer.go
git commit -m "feat(mcpsrv): one exported door to a tool handler

Invoke always runs policy.Decide, then dispatch-or-refuse, then
audit.Append; there is no branch that skips the first or the last. The
registry is unexported and the invariant is asserted structurally by
parsing this package's own source, because you cannot observe the absence
of a code path by calling things.

Trust is stamped from registration inside dispatch, so a handler cannot
launder untrusted content as trusted. An approver that errors produces a
CRITICAL record and a refusal carrying a message Hermes can repeat to the
user — the agent becomes the out-of-band alarm exactly when the safety
channel is what is broken."
```

---

### Task 6: `internal/tools` — the four stub tools

None of them can do harm. That is the point: the boundary gets exercised before anything valuable sits behind it.

**Files:**
- Create: `internal/tools/tools.go`
- Test: `internal/tools/tools_test.go`

**Interfaces:**
- Consumes: `mcpsrv.Registration`, `mcpsrv.Trusted`, `policy.TierInternal`, `policy.Read`, `policy.Write` (Tasks 1, 5); `state.Store` (Task 4).
- Produces: `func All(deps Deps) []mcpsrv.Registration` where `type Deps struct { Version string; MemoryRoot string; State *state.Store; Config policy.Config; Describe func() []mcpsrv.Registration }`.

`Describe` is a function rather than a value because `capabilities` needs the registration list that `All` is in the middle of producing. `cmd/chhayad` closes over the gateway and passes `g.Registrations`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tools/tools_test.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

func deps(t *testing.T) Deps {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "taste"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "taste", "food.md"), []byte("# Food\n\nNo coriander.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Version:    "0.1.0-test",
		MemoryRoot: root,
		State:      st,
		Config: policy.Config{
			Caps:               map[string]policy.Cap{"food": {PerOrderMinor: 80000, PerDayMinor: 150000}},
			DefaultRatePerHour: 60,
		},
		Describe: func() []mcpsrv.Registration { return nil },
	}
}

func find(t *testing.T, regs []mcpsrv.Registration, name string) mcpsrv.Registration {
	t.Helper()
	for _, r := range regs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no tool named %q", name)
	return mcpsrv.Registration{}
}

// TestTheRegisteredSetIsExactlyWhatWeExpect is the control that makes "a tool
// with no tier is a bug" enforceable. Adding a tool without deciding its tier,
// kind and trust fails here, by name.
func TestTheRegisteredSetIsExactlyWhatWeExpect(t *testing.T) {
	want := map[string]struct {
		tier  policy.Tier
		kind  policy.Kind
		trust mcpsrv.Trust
	}{
		"ping":                  {policy.TierInternal, policy.Read, mcpsrv.Trusted},
		"memory.get":            {policy.TierInternal, policy.Read, mcpsrv.Trusted},
		"memory.propose_update": {policy.TierInternal, policy.Write, mcpsrv.Trusted},
		"capabilities":          {policy.TierInternal, policy.Read, mcpsrv.Trusted},
	}

	got := All(deps(t))
	if len(got) != len(want) {
		t.Fatalf("registered %d tools, expected %d — if you added one, add it here too", len(got), len(want))
	}
	for _, r := range got {
		w, ok := want[r.Name]
		if !ok {
			t.Errorf("%s is registered but not expected; decide its tier, kind and trust here", r.Name)
			continue
		}
		if r.Tier != w.tier || r.Kind != w.kind || r.Trust != w.trust {
			t.Errorf("%s: tier/kind/trust = %d/%v/%q, want %d/%v/%q",
				r.Name, r.Tier, r.Kind, r.Trust, w.tier, w.kind, w.trust)
		}
		if r.Description == "" {
			t.Errorf("%s: no description — the agent should plan within a tool's limits, not discover them by rejection", r.Name)
		}
	}
}

func TestPingReturnsVersionAndTime(t *testing.T) {
	d := deps(t)
	out, err := find(t, All(d), "ping").Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["pong"] != true {
		t.Errorf("pong = %v", m["pong"])
	}
	if m["version"] != "0.1.0-test" {
		t.Errorf("version = %v", m["version"])
	}
	if _, err := time.Parse(time.RFC3339, m["time"].(string)); err != nil {
		t.Errorf("time = %v: %v", m["time"], err)
	}
}

func TestMemoryGetReadsAFile(t *testing.T) {
	d := deps(t)
	out, err := find(t, All(d), "memory.get").Handle(context.Background(), map[string]any{"path": "taste/food"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.(string), "No coriander") {
		t.Errorf("got %q", out)
	}
}

func TestMemoryGetRefusesToEscapeTheMemoryRoot(t *testing.T) {
	d := deps(t)
	h := find(t, All(d), "memory.get").Handle
	for _, bad := range []string{"../../../etc/passwd", "/etc/passwd", "taste/../../outside", "taste/food/../../.."} {
		t.Run(bad, func(t *testing.T) {
			if _, err := h(context.Background(), map[string]any{"path": bad}); err == nil {
				t.Fatalf("%q escaped the memory root", bad)
			}
		})
	}
}

func TestProposeUpdateWritesAProposalNotTheFile(t *testing.T) {
	d := deps(t)
	before, err := os.ReadFile(filepath.Join(d.MemoryRoot, "taste", "food.md"))
	if err != nil {
		t.Fatal(err)
	}

	out, err := find(t, All(d), "memory.propose_update").Handle(context.Background(), map[string]any{
		"path": "taste/food", "diff": "+ No mushrooms either.", "rationale": "said so on Tuesday",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := out.(map[string]any)["proposal_id"].(string)
	if id == "" {
		t.Fatal("no proposal id")
	}

	after, _ := os.ReadFile(filepath.Join(d.MemoryRoot, "taste", "food.md"))
	if string(before) != string(after) {
		t.Fatal("propose_update edited memory directly; it must only propose")
	}

	b, err := os.ReadFile(filepath.Join(d.MemoryRoot, ".proposals", id+".json"))
	if err != nil {
		t.Fatalf("the proposal was not persisted: %v", err)
	}
	var p map[string]any
	json.Unmarshal(b, &p)
	if p["rationale"] != "said so on Tuesday" {
		t.Errorf("proposal = %v; provenance is the point of the flow", p)
	}
}

func TestProposeUpdateRequiresARationale(t *testing.T) {
	d := deps(t)
	h := find(t, All(d), "memory.propose_update").Handle
	if _, err := h(context.Background(), map[string]any{"path": "taste/food", "diff": "+x"}); err == nil {
		t.Fatal("every memory entry records its provenance; a proposal without one is not a proposal")
	}
}

func TestCapabilitiesReportsStateHonestly(t *testing.T) {
	d := deps(t)
	d.State.SetKillSwitch(true, time.Now())
	d.State.RecordSpend("food", 40000, time.Now())

	out, err := find(t, All(d), "capabilities").Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["kill_switch"] != true {
		t.Error("capabilities must admit the kill switch is on; that is what it is for")
	}
	caps := m["caps"].(map[string]any)["food"].(map[string]any)
	if caps["remaining_today_inr"] != int64(1100) {
		t.Errorf("remaining = %v, want 1100", caps["remaining_today_inr"])
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tools/...`
Expected: FAIL — `undefined: All`, `undefined: Deps`.

- [ ] **Step 3: Implement**

Create `internal/tools/tools.go`:

```go
// Package tools holds the gateway's tool handlers. In M0b all four are stubs
// or reads: the boundary is exercised before anything valuable sits behind it.
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

type Deps struct {
	Version    string
	MemoryRoot string
	State      *state.Store
	Config     policy.Config
	// Describe returns tool metadata for capabilities(). It is a function
	// because capabilities needs the list All() is in the middle of building.
	Describe func() []mcpsrv.Registration
}

const objectSchema = `{"type":"object"}`

func All(d Deps) []mcpsrv.Registration {
	return []mcpsrv.Registration{
		{
			Name:        "ping",
			Description: "Liveness probe. Returns the gateway version and its clock. No side effects, no limits beyond the shared rate limit.",
			Tier:        policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(objectSchema),
			Handle: func(context.Context, map[string]any) (any, error) {
				return map[string]any{"pong": true, "version": d.Version, "time": time.Now().UTC().Format(time.RFC3339)}, nil
			},
		},
		{
			Name: "memory.get",
			Description: "Read one Personal memory document by path, e.g. \"taste/food\". " +
				"Returns Markdown. Paths are relative to the memory root and cannot escape it.",
			Tier: policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"document path, e.g. taste/food"}},"required":["path"]}`),
			Handle: func(_ context.Context, args map[string]any) (any, error) {
				p, err := memoryPath(d.MemoryRoot, str(args["path"]))
				if err != nil {
					return nil, err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, fmt.Errorf("no memory document at %q", str(args["path"]))
				}
				return string(b), nil
			},
		},
		{
			Name: "memory.propose_update",
			Description: "Propose a change to a Personal memory document. This does NOT write the document; " +
				"it records a proposal for review. A rationale is required — every memory entry records why it exists.",
			Tier: policy.TierInternal, Kind: policy.Write, Trust: mcpsrv.Trusted,
			Redact: []string{"diff"},
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"document path, e.g. taste/food"},` +
				`"diff":{"type":"string","description":"the proposed change"},` +
				`"rationale":{"type":"string","description":"why this change; recorded as provenance"}},` +
				`"required":["path","diff","rationale"]}`),
			Handle: func(_ context.Context, args map[string]any) (any, error) {
				path, diff, why := str(args["path"]), str(args["diff"]), str(args["rationale"])
				if path == "" || diff == "" {
					return nil, fmt.Errorf("path and diff are both required")
				}
				if why == "" {
					return nil, fmt.Errorf("a rationale is required: every memory entry records its provenance")
				}
				if _, err := memoryPath(d.MemoryRoot, path); err != nil {
					return nil, err
				}
				id, err := newID()
				if err != nil {
					return nil, err
				}
				dir := filepath.Join(d.MemoryRoot, ".proposals")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, err
				}
				body, _ := json.Marshal(map[string]any{
					"proposal_id": id, "path": path, "diff": diff,
					"rationale": why, "proposed_at": time.Now().UTC().Format(time.RFC3339),
				})
				if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o600); err != nil {
					return nil, err
				}
				return map[string]any{"proposal_id": id, "status": "recorded for review"}, nil
			},
		},
		{
			Name: "capabilities",
			Description: "What this gateway can do right now: caps remaining today, whether the kill switch is on, " +
				"whether the session is tainted, and every tool's tier. Ask before promising the user something.",
			Tier: policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(objectSchema),
			Handle: func(context.Context, map[string]any) (any, error) {
				now := time.Now()
				st := d.State.Snapshot(now)

				caps := map[string]any{}
				for key, c := range d.Config.Caps {
					left := c.PerDayMinor - st.Spent[key]
					if left < 0 {
						left = 0
					}
					caps[key] = map[string]any{
						"per_order_inr":       c.PerOrderMinor / 100,
						"remaining_today_inr": left / 100,
					}
				}

				var tools []map[string]any
				for _, r := range d.Describe() {
					tools = append(tools, map[string]any{
						"name": r.Name, "tier": int(r.Tier), "kind": r.Kind.String(), "trust": string(r.Trust),
					})
				}

				return map[string]any{
					"kill_switch":     st.KillSwitch,
					"tainted":         st.Tainted,
					"taint_source":    st.TaintSource,
					"rate_limit_hour": d.Config.DefaultRatePerHour,
					"caps":            caps,
					"tools":           tools,
				}, nil
			},
		},
	}
}

// memoryPath resolves a document path inside the memory root and refuses
// anything that leaves it. Symlinks are resolved before the check, so a link
// planted inside the root cannot be used to read outside it.
func memoryPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path must be relative to the memory root")
	}
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(absRoot, filepath.Clean(rel)+".md")
	resolved := p
	if r, err := filepath.EvalSymlinks(p); err == nil {
		resolved = r
	}
	if resolved != absRoot && !strings.HasPrefix(resolved, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}
	return p, nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(b), nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/tools/... -v`
Expected: PASS.

`TestCapabilitiesReportsStateHonestly` compares against `int64(1100)`: ₹1500 cap − ₹400 spent. If it fails with a type mismatch, the division is returning `int64` as written — keep it; don't weaken the assertion to `any`.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/
git commit -m "feat(tools): ping, memory.get, memory.propose_update, capabilities

propose_update records a proposal and never touches the document, and
refuses without a rationale, because every memory entry records its
provenance. memory.get resolves symlinks before the containment check, so
a link planted inside the root cannot read outside it.

The registered-set test is the control that makes 'a tool with no tier is
a bug' enforceable: adding a tool without stating its tier, kind and trust
fails by name."
```

---

### Task 7: `internal/approval` — the approver bot

The only process holding the approver token. Hermes has no path to it, which is what makes approvals and the kill switch unforgeable.

**Files:**
- Create: `internal/approval/telegram.go`
- Create: `internal/approval/bot.go`
- Test: `internal/approval/bot_test.go`

**Interfaces:**
- Consumes: `mcpsrv.Approver`, `mcpsrv.ToolCall`, `policy.Decision` (Tasks 1, 5); `state.Store` (Task 4); `audit.Writer` (Task 3).
- Produces:
  - `type Telegram interface` as below
  - `func NewHTTPTelegram(token string, client *http.Client) Telegram`
  - `func NewBot(tg Telegram, chatID int64, st *state.Store, w *audit.Writer, timeout time.Duration) *Bot`
  - `func (b *Bot) Run(ctx context.Context) error` — the long-poll loop
  - `func (b *Bot) Request(...)` — satisfies `mcpsrv.Approver`
  - `func (b *Bot) Preflight(ctx context.Context) error` — `getMe` then `getChat`
  - `func (b *Bot) SendStartupCard(ctx context.Context, policyHash string, firstBoot bool) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/approval/bot_test.go`:

```go
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

const allowedUser = int64(8726466304)

type fakeTG struct {
	mu        sync.Mutex
	sent      []string
	updates   []Update
	sendErr   error
	getChatOK bool
	answered  []string
	// reply, if set, is the callback this fake produces the moment a card is sent.
	reply func(messageID int64) *Update
	out   chan Update
}

func newFakeTG() *fakeTG {
	return &fakeTG{getChatOK: true, out: make(chan Update, 16)}
}

func (f *fakeTG) GetMe(context.Context) (int64, string, error) { return 8940896193, "satish_chhaya_approver_bot", nil }

func (f *fakeTG) GetChat(_ context.Context, _ int64) error {
	if !f.getChatOK {
		return errors.New("chat not found")
	}
	return nil
}

func (f *fakeTG) SendMessage(_ context.Context, _ int64, text string, _ [][]Button) (int64, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.mu.Lock()
	f.sent = append(f.sent, text)
	id := int64(len(f.sent))
	f.mu.Unlock()
	if f.reply != nil {
		if u := f.reply(id); u != nil {
			f.out <- *u
		}
	}
	return id, nil
}

func (f *fakeTG) AnswerCallback(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, id)
	return nil
}

func (f *fakeTG) GetUpdates(ctx context.Context, _ int64, timeout time.Duration) ([]Update, error) {
	select {
	case u := <-f.out:
		return []Update{u}, nil
	case <-time.After(timeout):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTG) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

func newTestBot(t *testing.T, tg Telegram, timeout time.Duration) (*Bot, *state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "audit.jsonl")
	w, err := audit.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	b := NewBot(tg, allowedUser, st, w, timeout)
	return b, st, logPath
}

func cmd(text string) Update {
	return Update{ID: time.Now().UnixNano(), Message: &Message{From: User{ID: allowedUser}, Chat: Chat{ID: allowedUser}, Text: text}}
}

func TestStopAndResumeDriveTheKillSwitch(t *testing.T) {
	tg := newFakeTG()
	b, st, _ := newTestBot(t, tg, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	tg.out <- cmd("/stop")
	waitFor(t, func() bool { return st.Snapshot(time.Now()).KillSwitch })

	tg.out <- cmd("/resume")
	waitFor(t, func() bool { return !st.Snapshot(time.Now()).KillSwitch })
}

func TestClearClearsTaint(t *testing.T) {
	tg := newFakeTG()
	b, st, _ := newTestBot(t, tg, time.Second)
	st.SetTaint(true, "web.fetch", time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	tg.out <- cmd("/clear")
	waitFor(t, func() bool { return !st.Snapshot(time.Now()).Tainted })
}

func TestAnUnauthorisedSenderIsIgnoredAndAudited(t *testing.T) {
	tg := newFakeTG()
	b, st, logPath := newTestBot(t, tg, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	u := cmd("/stop")
	u.Message.From.ID = 999 // not the configured user
	tg.out <- u

	waitFor(t, func() bool { return len(records(t, logPath)) > 0 })
	if st.Snapshot(time.Now()).KillSwitch {
		t.Fatal("a stranger pressed the kill switch")
	}
	last := records(t, logPath)[0]
	if last.Rule != "approver.unauthorized_sender" {
		t.Errorf("rule = %q", last.Rule)
	}
	if !strings.Contains(string(last.Args), "999") {
		t.Error("the record should say who it was, so we can see it happening")
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	tg := newFakeTG() // reply is nil: nobody taps
	b, _, logPath := newTestBot(t, tg, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	approved, latency, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write", Reason: "tainted"})
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		t.Fatal("a timeout must deny")
	}
	if latency < 150*time.Millisecond {
		t.Errorf("latency = %v, should cover the wait", latency)
	}

	recs := records(t, logPath)
	if len(recs) == 0 {
		t.Fatal("a timeout needs a trace as loud as a tap")
	}
	last := recs[len(recs)-1]
	if last.Approval == nil || last.Approval.Answer != "timeout" {
		t.Errorf("record = %+v", last)
	}
}

func TestApprovalTapIsHonoured(t *testing.T) {
	tg := newFakeTG()
	tg.reply = func(messageID int64) *Update {
		return &Update{ID: messageID, Callback: &Callback{
			ID: "cb1", From: User{ID: allowedUser}, Data: "approve:" + callbackToken(messageID),
		}}
	}
	b, _, _ := newTestBot(t, tg, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	approved, _, err := b.Request(ctx, mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write", Reason: "tainted"})
	if err != nil {
		t.Fatal(err)
	}
	if !approved {
		t.Fatal("the tap should have approved it")
	}
	if len(tg.answered) == 0 {
		t.Error("answerCallbackQuery closes the loop; without it the button spins forever")
	}
}

func TestACardStatesTheRuleThatFired(t *testing.T) {
	tg := newFakeTG()
	b, _, _ := newTestBot(t, tg, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	b.Request(ctx, mcpsrv.ToolCall{Name: "food.place_order", Args: map[string]any{"total_inr": 420}},
		policy.Decision{Outcome: policy.NeedApproval, Tier: policy.TierMoney, RuleID: "tier2.approval",
			Reason: "approval required: this spends money"})

	texts := tg.texts()
	if len(texts) == 0 {
		t.Fatal("no card was sent")
	}
	card := texts[0]
	for _, want := range []string{"food.place_order", "tier2.approval"} {
		if !strings.Contains(card, want) {
			t.Errorf("card does not mention %q:\n%s", want, card)
		}
	}
}

func TestAFailedSendIsReportedAsAnError(t *testing.T) {
	tg := newFakeTG()
	tg.sendErr = errors.New("forbidden: bot was blocked by the user")
	b, _, _ := newTestBot(t, tg, time.Second)

	_, _, err := b.Request(context.Background(), mcpsrv.ToolCall{Name: "probe.write"},
		policy.Decision{Outcome: policy.NeedApproval, RuleID: "taint.write"})
	if err == nil {
		t.Fatal("a card that could not be sent must surface as an error, not a silent deny")
	}
}

func TestPreflightRefusesAnUnreachableChat(t *testing.T) {
	tg := newFakeTG()
	tg.getChatOK = false
	b, _, _ := newTestBot(t, tg, time.Second)

	if err := b.Preflight(context.Background()); err == nil {
		t.Fatal("an unreachable approver chat must stop the gateway starting")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func records(t *testing.T, path string) []audit.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
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
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/approval/...`
Expected: FAIL — `undefined: NewBot`, `undefined: Update`, `undefined: Telegram`.

- [ ] **Step 3: Implement the Telegram transport**

Create `internal/approval/telegram.go`:

```go
// Package approval owns the approver bot: the only process holding the
// approver token. Hermes has no path to it, which is what makes approvals and
// the kill switch unforgeable.
package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type User struct {
	ID int64 `json:"id"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type Message struct {
	ID   int64  `json:"message_id"`
	From User   `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

type Callback struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}

type Button struct {
	Text string
	Data string
}

// Telegram is the four calls we make. An interface so the bot tests need no
// network, and so no Telegram library enters a security component.
type Telegram interface {
	GetMe(ctx context.Context) (botID int64, username string, err error)
	GetChat(ctx context.Context, chatID int64) error
	SendMessage(ctx context.Context, chatID int64, text string, buttons [][]Button) (messageID int64, err error)
	AnswerCallback(ctx context.Context, callbackID, text string) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
}

type httpTG struct {
	token  string
	client *http.Client
}

func NewHTTPTelegram(token string, client *http.Client) Telegram {
	if client == nil {
		client = &http.Client{Timeout: 70 * time.Second} // longer than the poll
	}
	return &httpTG{token: token, client: client}
}

// call never includes the token in an error, because errors get logged.
func (t *httpTG) call(ctx context.Context, method string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + t.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("telegram %s: build request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("telegram %s: %v", method, ue.Err) // ue.Err has no URL, so no token
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()

	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: unreadable response", method)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (t *httpTG) GetMe(ctx context.Context) (int64, string, error) {
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return 0, "", err
	}
	return me.ID, me.Username, nil
}

func (t *httpTG) GetChat(ctx context.Context, chatID int64) error {
	return t.call(ctx, "getChat", map[string]any{"chat_id": chatID}, nil)
}

func (t *httpTG) SendMessage(ctx context.Context, chatID int64, text string, buttons [][]Button) (int64, error) {
	body := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"}
	if len(buttons) > 0 {
		rows := make([][]map[string]string, 0, len(buttons))
		for _, row := range buttons {
			r := make([]map[string]string, 0, len(row))
			for _, b := range row {
				r = append(r, map[string]string{"text": b.Text, "callback_data": b.Data})
			}
			rows = append(rows, r)
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	var msg Message
	if err := t.call(ctx, "sendMessage", body, &msg); err != nil {
		return 0, err
	}
	return msg.ID, nil
}

func (t *httpTG) AnswerCallback(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (t *httpTG) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	// allowed_updates must be a JSON array, not a comma string. Getting this
	// wrong during the spike produced "no callback" while three taps sat in
	// the queue — the error response parsed as an empty result.
	body := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}
	if err := t.call(ctx, "getUpdates", body, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}
```

- [ ] **Step 4: Implement the bot**

Create `internal/approval/bot.go`:

```go
package approval

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

// callbackSecret salts the token embedded in each button so that a
// callback_data value cannot be guessed or replayed across restarts.
var callbackSecret = func() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}()

func callbackToken(messageID int64) string {
	m := hmac.New(sha256.New, callbackSecret)
	fmt.Fprintf(m, "%d", messageID)
	return hex.EncodeToString(m.Sum(nil))[:16]
}

type pending struct {
	ch    chan bool
	rule  string
	tool  string
	start time.Time
}

type Bot struct {
	tg      Telegram
	chatID  int64
	state   *state.Store
	audit   *audit.Writer
	timeout time.Duration

	mu      sync.Mutex
	waiting map[int64]*pending // keyed by the card's message id
}

// NewBot wires the approver bot. chatID is both the chat we post to and the
// only user whose taps count: in a private Telegram chat the chat id and the
// user id are the same number, and collapsing them means there is no way to
// configure a chat we listen to but do not trust.
func NewBot(tg Telegram, chatID int64, st *state.Store, w *audit.Writer, timeout time.Duration) *Bot {
	return &Bot{tg: tg, chatID: chatID, state: st, audit: w, timeout: timeout, waiting: map[int64]*pending{}}
}

// Preflight is startup steps 3 and 4: the token must work, and the chat must
// be reachable. Either failing means the gateway does not start.
func (b *Bot) Preflight(ctx context.Context) error {
	if _, _, err := b.tg.GetMe(ctx); err != nil {
		return fmt.Errorf("approver bot token rejected: %w", err)
	}
	if err := b.tg.GetChat(ctx, b.chatID); err != nil {
		return fmt.Errorf("approver chat %d unreachable; press Start on the approver bot: %w", b.chatID, err)
	}
	return nil
}

// SendStartupCard posts only on first boot or when the policy changed. A card
// you see on every bounce is one you stop reading, and this bot's messages
// must stay worth reading.
func (b *Bot) SendStartupCard(ctx context.Context, policyHash string, firstBoot bool) error {
	why := "policy changed"
	if firstBoot {
		why = "first boot"
	}
	_, err := b.tg.SendMessage(ctx, b.chatID,
		fmt.Sprintf("<b>chhaya gateway up</b> (%s)\npolicy <code>%s</code>\n/status for caps and switches", why, policyHash[:12]), nil)
	return err
}

// Run long-polls for commands and button taps until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ups, err := b.tg.GetUpdates(ctx, offset, 25*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(2 * time.Second) // transient; keep polling
			continue
		}
		for _, u := range ups {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			b.handle(ctx, u)
		}
	}
}

func (b *Bot) handle(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		if u.Message.From.ID != b.chatID {
			b.auditRejectedSender(u.Message.From.ID, "message", u.Message.Text)
			return
		}
		b.command(ctx, strings.TrimSpace(u.Message.Text))
	case u.Callback != nil:
		if u.Callback.From.ID != b.chatID {
			b.auditRejectedSender(u.Callback.From.ID, "callback", u.Callback.Data)
			return
		}
		b.tap(ctx, u.Callback)
	}
}

func (b *Bot) command(ctx context.Context, text string) {
	now := time.Now()
	switch {
	case strings.HasPrefix(text, "/stop"):
		if err := b.state.SetKillSwitch(true, now); err != nil {
			b.tg.SendMessage(ctx, b.chatID, "could not persist the kill switch: "+err.Error(), nil)
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>stopped.</b> Writes are refused. Reads still work so it can tell you why. /resume to lift.", nil)

	case strings.HasPrefix(text, "/resume"):
		if err := b.state.SetKillSwitch(false, now); err != nil {
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>resumed.</b> Writes are allowed again.", nil)

	case strings.HasPrefix(text, "/clear"):
		if err := b.state.SetTaint(false, "", now); err != nil {
			return
		}
		b.tg.SendMessage(ctx, b.chatID, "<b>taint cleared.</b> Writes no longer need approval on that account.", nil)

	case strings.HasPrefix(text, "/status"):
		st := b.state.Snapshot(now)
		var sb strings.Builder
		fmt.Fprintf(&sb, "kill switch: <b>%s</b>\n", onOff(st.KillSwitch))
		if st.Tainted {
			fmt.Fprintf(&sb, "tainted: <b>yes</b>, from %s at %s\n", st.TaintSource, st.TaintedAt.UTC().Format(time.RFC3339))
		} else {
			sb.WriteString("tainted: no\n")
		}
		for k, v := range st.Spent {
			fmt.Fprintf(&sb, "spent on %s today: ₹%d\n", k, v/100)
		}
		b.tg.SendMessage(ctx, b.chatID, sb.String(), nil)
	}
}

func (b *Bot) tap(ctx context.Context, c *Callback) {
	parts := strings.SplitN(c.Data, ":", 2)
	if len(parts) != 2 || c.Message == nil {
		return
	}
	if parts[1] != callbackToken(c.Message.ID) {
		b.tg.AnswerCallback(ctx, c.ID, "that card has expired")
		return
	}

	b.mu.Lock()
	p, ok := b.waiting[c.Message.ID]
	if ok {
		delete(b.waiting, c.Message.ID)
	}
	b.mu.Unlock()
	if !ok {
		b.tg.AnswerCallback(ctx, c.ID, "already answered")
		return
	}

	approved := parts[0] == "approve"
	b.tg.AnswerCallback(ctx, c.ID, map[bool]string{true: "approved", false: "denied"}[approved])
	p.ch <- approved
}

// Request implements mcpsrv.Approver.
func (b *Bot) Request(ctx context.Context, call mcpsrv.ToolCall, d policy.Decision) (bool, time.Duration, error) {
	start := time.Now()

	text := b.card(call, d)
	msgID, err := b.tg.SendMessage(ctx, b.chatID, text, nil)
	if err != nil {
		// Do not guess. Surfacing the error lets Invoke write the CRITICAL
		// record and tell Hermes the channel is down.
		return false, time.Since(start), fmt.Errorf("could not send the approval card: %w", err)
	}

	tok := callbackToken(msgID)
	if _, err := b.tg.SendMessage(ctx, b.chatID, "approve?", [][]Button{{
		{Text: "✅ approve", Data: "approve:" + tok},
		{Text: "❌ deny", Data: "deny:" + tok},
	}}); err != nil {
		return false, time.Since(start), fmt.Errorf("could not send the approval buttons: %w", err)
	}

	p := &pending{ch: make(chan bool, 1), rule: d.RuleID, tool: call.Name, start: start}
	b.mu.Lock()
	b.waiting[msgID] = p
	b.mu.Unlock()

	select {
	case ok := <-p.ch:
		return ok, time.Since(start), nil

	case <-time.After(b.timeout):
		b.mu.Lock()
		delete(b.waiting, msgID)
		b.mu.Unlock()
		// A timeout needs a trace as loud as a tap, and it is this package's
		// event, not Invoke's: Invoke only sees "not approved".
		b.appendRecord(audit.Record{
			Actor: "approver", Tool: call.Name, Kind: "write",
			Args: json.RawMessage(`{}`), Decision: "deny", Rule: d.RuleID,
			Reason:   "no answer within " + b.timeout.String(),
			Approval: &audit.Approval{Answer: "timeout", LatencyMS: time.Since(start).Milliseconds()},
			Result:   audit.Result{Status: "refused"},
		})
		b.tg.SendMessage(ctx, b.chatID, "⏱ no answer in time — <b>denied</b>: "+call.Name, nil)
		return false, time.Since(start), nil

	case <-ctx.Done():
		return false, time.Since(start), ctx.Err()
	}
}

func (b *Bot) card(call mcpsrv.ToolCall, d policy.Decision) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>approval needed</b>\n")
	fmt.Fprintf(&sb, "tool: <code>%s</code>\n", call.Name)
	fmt.Fprintf(&sb, "tier: %d\n", d.Tier)
	fmt.Fprintf(&sb, "rule: <code>%s</code>\n", d.RuleID)
	if d.Reason != "" {
		fmt.Fprintf(&sb, "why: %s\n", d.Reason)
	}
	if len(call.Args) > 0 {
		shown, _ := json.Marshal(audit.Redact(call.Args, d.Redact))
		fmt.Fprintf(&sb, "args: <code>%s</code>", truncate(string(shown), 500))
	}
	return sb.String()
}

func (b *Bot) auditRejectedSender(senderID int64, kind, payload string) {
	args := audit.MarshalArgs(map[string]any{"sender_id": senderID, "update_kind": kind, "payload": truncate(payload, 120)})
	b.appendRecord(audit.Record{
		Actor: "unknown", Tool: "approver." + kind, Kind: "write", Args: args,
		Decision: "deny", Rule: "approver.unauthorized_sender",
		Reason:   fmt.Sprintf("telegram user %d is not the configured approver", senderID),
		Result:   audit.Result{Status: "refused"},
		Severity: "critical",
	})
}

func (b *Bot) appendRecord(r audit.Record) {
	if _, err := b.audit.Append(r); err != nil {
		fmt.Printf("CRITICAL: approver could not write an audit record: %v\n", err)
	}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "off"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/approval/... -race -v`
Expected: PASS. Run with `-race`: this is the one package with a goroutine, a channel and a map under a mutex.

- [ ] **Step 6: Commit**

```bash
git add internal/approval/
git commit -m "feat(approval): approver bot, kill switch, timeout-as-deny

Timeout denies and writes its own audit record: an unanswered card should
leave a trace as loud as a tap. An unauthorised sender is ignored and
recorded as CRITICAL — we want to see that happening, not just not act on
it. A card that cannot be sent returns an error rather than a quiet deny,
so Invoke can write the CRITICAL record and tell Hermes the channel is
down.

Button callbacks carry an HMAC of the message id, so a callback_data
value cannot be guessed or replayed. allowed_updates is sent as a JSON
array: getting that wrong during the spike reported 'no callback' while
three taps sat in the queue."
```

---

### Task 8: `internal/mcpsrv/server.go` + `cmd/chhayad` — mTLS transport and the startup sequence

**Files:**
- Create: `internal/mcpsrv/server.go`
- Create: `internal/mcpsrv/server_test.go`
- Create: `cmd/chhayad/main.go`
- Modify: `go.mod` (adds the MCP SDK)

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces:
  - `func (g *Gateway) MCPHandler(name, version string) http.Handler`
  - `func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error)`
  - `func CheckEnvironmentIsolation(getenv func(string) string) error`

- [ ] **Step 1: Add the SDK**

```bash
go get github.com/modelcontextprotocol/go-sdk@v1.8.0
```

Expect `go: upgraded go 1.24 => 1.25.0` and a one-time toolchain download. Confirm with `go list -m -f '{{.GoVersion}}' github.com/modelcontextprotocol/go-sdk` → `1.25.0`.

- [ ] **Step 2: Write the failing tests**

Create `internal/mcpsrv/server_test.go`:

```go
package mcpsrv

import (
	"testing"
)

func TestCheckEnvironmentIsolation(t *testing.T) {
	const approver = "8940896193:AA-approver"

	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		mentions string
	}{
		{
			name: "the normal case",
			env:  map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver},
		},
		{
			name:     "Hermes' token is visible here",
			env:      map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": "8979753688:AA-hermes"},
			wantErr:  true,
			mentions: "TELEGRAM_BOT_TOKEN",
		},
		{
			name:     "Hermes' token is present but empty — still the wrong env file",
			env:      map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": ""},
			wantErr:  false, // an empty value is indistinguishable from unset via Getenv
		},
		{
			name:    "no approver token at all",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "the two tokens are the same",
			env:     map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": approver},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckEnvironmentIsolation(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.mentions != "" && err != nil && !contains(err.Error(), tc.mentions) {
				t.Errorf("error should name the variable: %v", err)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/mcpsrv/ -run TestCheckEnvironmentIsolation`
Expected: FAIL — `undefined: CheckEnvironmentIsolation`.

- [ ] **Step 4: Implement the server**

Create `internal/mcpsrv/server.go`:

```go
package mcpsrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CheckEnvironmentIsolation is startup step 0. TELEGRAM_BOT_TOKEN belongs to
// Hermes; its presence in the gateway's environment means the wrong env file
// was mounted and the two bots' secrets have been crossed. The check needs no
// knowledge of any token's value — presence alone is the error.
//
// This is the one configuration mistake that would silently void the safety
// model, so it is the one that must fail loudly at boot. A rule enforced in
// code beats a rule written in a comment.
func CheckEnvironmentIsolation(getenv func(string) string) error {
	approver := getenv("CHHAYA_APPROVER_BOT_TOKEN")
	hermes := getenv("TELEGRAM_BOT_TOKEN")

	if hermes != "" {
		return errors.New("TELEGRAM_BOT_TOKEN is set in the gateway's environment. " +
			"That variable belongs to Hermes; its presence here means deploy/.env.hermes was mounted into the gateway. " +
			"If the approver token ever reaches Hermes, the agent can forge its own approvals")
	}
	if approver == "" {
		return errors.New("CHHAYA_APPROVER_BOT_TOKEN is not set; without it there is no approval channel and no kill switch")
	}
	if approver == hermes {
		return errors.New("the approver token and Hermes' token are the same value; the two bots must be different bots")
	}
	return nil
}

// TLSConfig builds the server side of mTLS. Hermes presents a client
// certificate signed by our CA, or it does not get in.
func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// MCPHandler exposes the registry over MCP. Every tool is registered with the
// same handler shape: unmarshal, call Invoke, marshal. No tool gets its own
// path to its handler, so the choke point survives contact with the SDK.
func (g *Gateway) MCPHandler(name, version string) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)

	for _, reg := range g.tools {
		reg := reg
		srv.AddTool(&mcp.Tool{
			Name:        reg.Name,
			Description: reg.Description,
			InputSchema: reg.Schema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errorResult("arguments were not a JSON object"), nil
				}
			}

			res := g.Invoke(ctx, ToolCall{Actor: "hermes", Name: req.Params.Name, Args: args})
			if res.IsError {
				return errorResult(res.Error), nil
			}

			body, err := json.Marshal(map[string]any{"trust": string(res.Trust), "value": res.Value})
			if err != nil {
				return errorResult("result was not serialisable"), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
		})
	}

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

// errorResult returns a tool error rather than a protocol error, so the model
// can see the refusal and correct itself instead of the call simply failing.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
```

Note that `MCPHandler` is an exported method that reads `g.tools`. Add it to the allow-list in `TestRegistryIsUnreachableExceptThroughInvoke` **and** add this assertion next to it, so widening the list does not quietly weaken the invariant:

```go
// MCPHandler reads the registry to publish schemas, but must reach handlers
// only through Invoke. Assert that its body never calls reg.Handle.
func TestMCPHandlerDoesNotCallHandlersDirectly(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), ".Handle(") {
		t.Fatal("server.go calls a handler directly; it must go through Invoke")
	}
}
```

- [ ] **Step 5: Assert the gateway never asks the model anything**

Spec §3.4 requires the gateway to refuse sampling rather than trust Hermes' config alone. Sampling and elicitation are server-to-client calls, so "refusing" means never making one: if this process never calls `CreateMessage` or `Elicit`, there is nothing for a misconfigured client to answer. That is a property of our source, so assert it there.

Append to `internal/mcpsrv/server_test.go`:

```go
// TestGatewayNeverSamplesOrElicits is the structural half of spec 3.4.
// Elicitation would surface approval prompts through the agent, defeating the
// separate approver bot; sampling would let a tool borrow the model mid-call.
// Both are disabled in Hermes' config, and both default to on — so this test
// is what stops us relying on a config file we do not own.
func TestGatewayNeverSamplesOrElicits(t *testing.T) {
	forbidden := []string{"CreateMessage(", "CreateMessageWithTools(", "Elicit(", "NotifyElicitationComplete("}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s calls %s; the gateway must never ask the model or the user anything "+
					"through the MCP session — approvals go to the approver bot", e.Name(), f)
			}
		}
	}
}
```

Add `"os"` and `"strings"` to the imports of `server_test.go`.

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/mcpsrv/... -v`
Expected: PASS.

- [ ] **Step 7: Write `cmd/chhayad`**

Create `cmd/chhayad/main.go`. This file is the startup sequence from spec §6, in order, with each step naming what it defends. It contains no logic of its own.

```go
// chhayad is the policy gateway: the only MCP server Hermes can see, and the
// only door to code, money and data.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/config"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/state"
	"github.com/satishthakur/chhaya/internal/tools"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		log.Fatalf("chhayad: %v", err)
	}
}

func run() error {
	dataDir := env("CHHAYA_DATA_DIR", "/var/lib/chhaya")
	policyFile := env("CHHAYA_POLICY_FILE", "/etc/chhaya/policy.yaml")
	memoryRoot := env("CHHAYA_MEMORY_ROOT", filepath.Join(dataDir, "memory"))
	addr := env("CHHAYA_LISTEN", "0.0.0.0:8443")

	// Step 0. The two bots' secrets must not have been crossed.
	if err := mcpsrv.CheckEnvironmentIsolation(os.Getenv); err != nil {
		return err
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CHHAYA_APPROVER_ALLOWED_USERS")), 10, 64)
	if err != nil {
		return fmt.Errorf("CHHAYA_APPROVER_ALLOWED_USERS must be one numeric Telegram user id: %w", err)
	}

	// Step 1. Policy. A missing value is the restrictive reading, not a default.
	cfg, policyHash, err := config.Load(policyFile)
	if err != nil {
		return err
	}
	log.Printf("policy loaded, hash %s", policyHash[:12])

	// Step 2. Audit log. A broken chain is a refusal, not a warning.
	w, err := audit.Open(filepath.Join(dataDir, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer w.Close()

	// Step 6 (early, because the bot needs it). Persisted kill switch and taint.
	st, err := state.Open(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return err
	}
	if st.Snapshot(time.Now()).KillSwitch {
		log.Print("kill switch is ON from a previous /stop; writes will be refused")
	}

	// Steps 3 and 4. The token must work and the chat must be reachable.
	bot := approval.NewBot(
		approval.NewHTTPTelegram(os.Getenv("CHHAYA_APPROVER_BOT_TOKEN"), nil),
		chatID, st, w, cfg.ApprovalTimeout,
	)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := bot.Preflight(ctx); err != nil {
		return err
	}

	// Step 5. A card only on first boot or a policy change.
	markFile := filepath.Join(dataDir, "policy.hash")
	prev, _ := os.ReadFile(markFile)
	if string(prev) != policyHash {
		if err := bot.SendStartupCard(ctx, policyHash, len(prev) == 0); err != nil {
			return fmt.Errorf("the approver chat accepted getChat but refused a message, "+
				"which usually means the bot is blocked: %w", err)
		}
		if err := os.WriteFile(markFile, []byte(policyHash), 0o600); err != nil {
			return err
		}
	}

	go func() {
		if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("approver bot stopped: %v", err)
		}
	}()

	// Build the gateway. Describe is a closure because capabilities needs the
	// registration list the gateway is being constructed from.
	var g *mcpsrv.Gateway
	regs := tools.All(tools.Deps{
		Version: version, MemoryRoot: memoryRoot, State: st, Config: cfg,
		Describe: func() []mcpsrv.Registration { return g.Registrations() },
	})
	g, err = mcpsrv.New(cfg, st, w, bot, regs)
	if err != nil {
		return err
	}

	// Step 7. Only now bind. Hermes may connect before this and will retry;
	// a gateway that accepts calls before it can ask for approval fails open.
	tlsCfg, err := mcpsrv.TLSConfig(
		env("CHHAYA_TLS_CERT", "/etc/chhaya/certs/gateway.crt"),
		env("CHHAYA_TLS_KEY", "/etc/chhaya/certs/gateway.key"),
		env("CHHAYA_TLS_CA", "/etc/chhaya/certs/ca.crt"),
	)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", g.MCPHandler("chhaya-gateway", version))
	srv := &http.Server{
		Addr: addr, Handler: mux, TLSConfig: tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()

	log.Printf("listening on %s with mTLS, %d tools registered", addr, len(regs))
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

Note what is *not* in this file: no hashing, no tier logic, no YAML. `cmd/chhayad` is wiring. If you find yourself adding a condition here, it belongs in `internal/policy` and its value belongs in `policy.yaml`.

- [ ] **Step 8: Build and sanity-check the fail-closed startup**

```bash
go build -o bin/chhayad ./cmd/chhayad

# The cross-contamination check must fire before anything else.
TELEGRAM_BOT_TOKEN=anything CHHAYA_APPROVER_BOT_TOKEN=x ./bin/chhayad; echo "exit=$?"
```

Expected: exits non-zero with a message naming `TELEGRAM_BOT_TOKEN`, and it happens before any file is read.

```bash
# No approver token at all.
./bin/chhayad; echo "exit=$?"
```

Expected: non-zero, naming `CHHAYA_APPROVER_BOT_TOKEN`.

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum internal/mcpsrv/server.go internal/mcpsrv/server_test.go cmd/chhayad/
git commit -m "feat(chhayad): mTLS transport and the fail-closed startup sequence

The startup order is the spec's, step for step, and each step names what
it defends. The listener binds last: a gateway that accepts calls before
it can ask for approval is a gateway that fails open.

CheckEnvironmentIsolation runs before anything is read. It needs no
knowledge of any token's value — TELEGRAM_BOT_TOKEN being present in the
gateway's environment at all means the wrong env file was mounted, and
that is the one configuration mistake that would silently void the whole
model."
```

---

### Task 9: `cmd/chhayactl` — verify, explain, stop

A separate binary so that auditing the log never runs inside the process that writes it, and so the kill switch has a path that does not need the daemon to be healthy.

**Files:**
- Create: `cmd/chhayactl/main.go`
- Test: `cmd/chhayactl/main_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/chhayactl/main_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
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
	if !strings.Contains(out.String(), "1") {
		t.Errorf("output should state how many records were checked: %q", out.String())
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

	if err := runVerify(p, &bytes.Buffer{}); err == nil {
		t.Fatal("verify passed on a log whose decision was flipped")
	}
}

func TestStopSetsTheKillSwitchWithoutTheDaemon(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	if err := runStop(p, true); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Snapshot(time.Now()).KillSwitch {
		t.Fatal("chhayactl stop must work when the daemon is not running; that is the backstop")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/chhayactl/...`
Expected: FAIL — `undefined: runVerify`, `undefined: runStop`.

- [ ] **Step 3: Implement**

Create `cmd/chhayactl/main.go`:

```go
// chhayactl is the operator's side of the gateway. It is a separate binary so
// that verifying the audit log never runs inside the process that writes it,
// and so the kill switch has a path that does not need the daemon to be healthy.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/config"
	"github.com/satishthakur/chhaya/internal/state"
)

func main() {
	dataDir := flag.String("data", env("CHHAYA_DATA_DIR", "/var/lib/chhaya"), "gateway data directory")
	policyFile := flag.String("policy", env("CHHAYA_POLICY_FILE", "/etc/chhaya/policy.yaml"), "policy file")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "audit":
		if len(args) < 2 || args[1] != "verify" {
			usage()
			os.Exit(2)
		}
		err = runVerify(filepath.Join(*dataDir, "audit.jsonl"), os.Stdout)
	case "stop":
		err = runStop(filepath.Join(*dataDir, "state.json"), true)
	case "resume":
		err = runStop(filepath.Join(*dataDir, "state.json"), false)
	case "policy":
		if len(args) < 2 || args[1] != "explain" {
			usage()
			os.Exit(2)
		}
		err = runExplain(*policyFile, os.Stdout)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "chhayactl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `chhayactl [-data DIR] [-policy FILE] <command>

  audit verify     walk the hash chain from genesis and report
  stop             set the kill switch without the daemon
  resume           clear the kill switch
  policy explain   print the values the gateway is enforcing
`)
}

func runVerify(path string, out io.Writer) error {
	n, err := audit.Verify(path)
	if err != nil {
		fmt.Fprintf(out, "FAILED after %d records\n", n)
		return err
	}
	fmt.Fprintf(out, "ok: %d records, chain intact from genesis\n", n)
	return nil
}

func runStop(statePath string, on bool) error {
	st, err := state.Open(statePath)
	if err != nil {
		return err
	}
	if err := st.SetKillSwitch(on, time.Now()); err != nil {
		return err
	}
	if on {
		fmt.Println("kill switch ON — writes will be refused; reads still work")
	} else {
		fmt.Println("kill switch off")
	}
	return nil
}

func runExplain(policyFile string, out io.Writer) error {
	cfg, hash, err := config.Load(policyFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "policy hash: %s\n", hash)
	fmt.Fprintf(out, "rate limit:  %d calls per tool per hour\n", cfg.DefaultRatePerHour)
	fmt.Fprintf(out, "approvals:   %s before a card times out and denies\n", cfg.ApprovalTimeout)
	fmt.Fprintf(out, "branches:    %q\n", cfg.BranchPrefix)
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(out, "repos:       none — no repository is writable")
	} else {
		fmt.Fprintf(out, "repos:       %v\n", cfg.Repos)
	}
	for key, c := range cfg.Caps {
		fmt.Fprintf(out, "cap %-8s ₹%d per order, ₹%d per day\n", key+":", c.PerOrderMinor/100, c.PerDayMinor/100)
	}
	fmt.Fprintln(out, "\nTiers are decided in internal/policy, not here. This file supplies values only.")
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./cmd/chhayactl/... -v && go build -o bin/chhayactl ./cmd/chhayactl`
Expected: PASS, and a binary.

- [ ] **Step 5: Commit**

```bash
git add cmd/chhayactl/
git commit -m "feat(chhayactl): audit verify, policy explain, stop

A separate binary on purpose: verifying the log never runs inside the
process that writes it, and 'stop' works when the daemon does not, which
is what makes it a backstop rather than a second front door."
```

---

### Task 10: Certificates, compose, and the exit check end to end

This is the task that produces M0b's deliverable: **a blocked write is logged.**

**Files:**
- Create: `deploy/gen-certs.sh`
- Create: `deploy/docker-compose.yml`
- Create: `deploy/Dockerfile.gateway`
- Create: `test/e2e/e2e_test.go`
- Modify: `Makefile`

- [ ] **Step 1: Write the certificate script**

Create `deploy/gen-certs.sh`:

```bash
#!/usr/bin/env bash
# Generate the CA, the gateway's server certificate and Hermes' client
# certificate. Run once; the output is gitignored (*.crt, *.key, certs/).
#
#   deploy/gen-certs.sh deploy/certs
#
# The CA is ours and signs exactly two certificates. Hermes presents its
# client cert or it does not reach the gateway at all.
set -euo pipefail

OUT="${1:-deploy/certs}"
mkdir -p "$OUT"
cd "$OUT"

if [ -f ca.key ]; then
  echo "certs already exist in $PWD — delete them first if you really mean to rotate"
  exit 1
fi

openssl req -x509 -newkey rsa:4096 -days 3650 -nodes \
  -keyout ca.key -out ca.crt -subj "/CN=chhaya-ca"

# Server. SAN must cover whatever Hermes puts in its mcp_servers url.
openssl req -newkey rsa:4096 -nodes -keyout gateway.key -out gateway.csr \
  -subj "/CN=chhaya-gateway"
openssl x509 -req -in gateway.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out gateway.crt -days 825 \
  -extfile <(printf "subjectAltName=DNS:gateway,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth")

# Client, for Hermes.
openssl req -newkey rsa:4096 -nodes -keyout hermes.key -out hermes.csr \
  -subj "/CN=hermes"
openssl x509 -req -in hermes.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out hermes.crt -days 825 \
  -extfile <(printf "extendedKeyUsage=clientAuth")

rm -f ./*.csr
chmod 600 ./*.key
echo "wrote CA, gateway and hermes certs to $PWD"
```

```bash
chmod +x deploy/gen-certs.sh && deploy/gen-certs.sh deploy/certs
```

Verify nothing is stageable: `git status --short deploy/certs` must show nothing.

- [ ] **Step 2: Write the Dockerfile and compose file**

Create `deploy/Dockerfile.gateway`:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/chhayad ./cmd/chhayad \
 && CGO_ENABLED=0 go build -o /out/chhayactl ./cmd/chhayactl

FROM alpine:3.21
RUN adduser -D -u 10001 chhaya
COPY --from=build /out/chhayad /usr/local/bin/chhayad
COPY --from=build /out/chhayactl /usr/local/bin/chhayactl
USER chhaya
ENTRYPOINT ["/usr/local/bin/chhayad"]
```

Create `deploy/docker-compose.yml`:

```yaml
# The gateway owns its own lifecycle. It is NOT a child of Hermes: as a child
# process, Hermes would own the lifecycle of the process holding the kill
# switch, and Hermes strips credential-named env vars from MCP subprocesses.
services:
  gateway:
    build:
      context: ..
      dockerfile: deploy/Dockerfile.gateway
    env_file: .env.gateway        # NEVER .env.hermes — chhayad refuses to start if it sees that file's variables
    environment:
      CHHAYA_DATA_DIR: /data
      CHHAYA_POLICY_FILE: /etc/chhaya/policy.yaml
      CHHAYA_MEMORY_ROOT: /data/memory
      CHHAYA_LISTEN: 0.0.0.0:8443
      CHHAYA_TLS_CERT: /etc/chhaya/certs/gateway.crt
      CHHAYA_TLS_KEY: /etc/chhaya/certs/gateway.key
      CHHAYA_TLS_CA: /etc/chhaya/certs/ca.crt
    volumes:
      - gateway-data:/data
      - ../config/policy.yaml:/etc/chhaya/policy.yaml:ro
      - ./certs:/etc/chhaya/certs:ro
    ports:
      - "127.0.0.1:8443:8443"     # localhost only; nothing about this is internet-facing
    restart: unless-stopped

  hermes:
    image: nousresearch/hermes-agent@sha256:1d33f6087fab79d580ffaec34ceed825e11d7bb8e803206be75a56d027c62538
    env_file: .env.hermes         # exactly one secret: Hermes' own bot token
    volumes:
      - hermes-data:/data         # NOT :ro — Hermes creates cron/ at init and fails on a read-only home
      - ./certs/ca.crt:/certs/ca.crt:ro
      - ./certs/hermes.crt:/certs/hermes.crt:ro
      - ./certs/hermes.key:/certs/hermes.key:ro
    environment:
      HERMES_HOME: /data
    depends_on:
      - gateway
    restart: unless-stopped

volumes:
  gateway-data:
  hermes-data:
```

- [ ] **Step 3: Write the end-to-end exit check**

Create `test/e2e/e2e_test.go`. It runs the real daemon against a fake Telegram, over real mTLS, with a real MCP client.

```go
//go:build e2e

// Package e2e is the M0b exit check: a blocked write is logged.
// Run with: go test -tags e2e ./test/e2e/...
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
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
```

- [ ] **Step 4: Write the in-process helper and the mTLS negative control**

Append to `test/e2e/e2e_test.go`:

```go
import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"

	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

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
		Args: map[string]any{"path": "taste/food", "diff": "+ nothing", "rationale": "exit check"},
	})
}

// TestClientWithoutACertificateIsRefused is the mTLS negative control. It is
// the half of "mTLS works" that is easy to forget to assert: that the door is
// actually locked, not just that your key opens it.
func TestClientWithoutACertificateIsRefused(t *testing.T) {
	certDir := "../../deploy/certs"
	if _, err := os.Stat(filepath.Join(certDir, "ca.crt")); err != nil {
		t.Skip("run deploy/gen-certs.sh first")
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
```

Add `"github.com/satishthakur/chhaya/internal/tools"` to the import block and merge the two import blocks into one.

- [ ] **Step 5: Add the e2e target and run everything**

Add to the `Makefile`:

```makefile
e2e:
	go test -tags e2e ./test/e2e/... -v
```

Run:

```bash
make test
make e2e
```

Expected: both pass. `make e2e` is the M0b exit check.

- [ ] **Step 6: Commit**

```bash
git add deploy/gen-certs.sh deploy/Dockerfile.gateway deploy/docker-compose.yml test/e2e/ Makefile
git commit -m "feat(e2e): the M0b exit check — a blocked write is logged

/stop, then memory.propose_update, then Deny(killswitch.active), then the
audit record, then chhayactl audit verify from a separate process. Real
tools throughout: a debug.write_probe would prove only that the probe
works.

The mTLS test asserts both halves, including the one that is easy to skip
— that a client with no certificate is refused, not just that ours is
accepted."
```

---

### Task 11: Close the remaining documentation debt

Spec §9 lists three items. Two were done on 2026-10-22; these are the rest.

**Files:**
- Modify: `docs/design.md` (the Hermes bootstrap section)
- Modify: `scripts/verify-hermes-image.sh`
- Modify: `docs/decisions.md` (append)

- [ ] **Step 1: Make the disabled web toolsets architectural in `design.md`**

Find the Hermes bootstrap section and the `mcp_servers` config block. Add, immediately after the `sampling`/`elicitation` note, a paragraph that says why the web toolsets are off — currently they read as a configuration preference and they are load-bearing:

```markdown
**Hermes' built-in web, browser and fetch toolsets are disabled, and that is
architectural rather than a preference.** The gateway cannot see a tool call it
does not serve, so anything Hermes fetches on its own is untrusted content that
arrives without passing the taint check. Taint tracking that is blind to the
likeliest injection vector while diligently tracking the ones it can see is
worse than none, because it reads as protection. Disabling them makes the
gateway the one door for untrusted *reads*, not just for writes. Any future web
access arrives as a gateway tool that marks its results untrusted.
```

- [ ] **Step 2: Convert the source-inspection checks to `grep`**

In `scripts/verify-hermes-image.sh`, replace the three `run "import inspect, ..."` checks with `grep` against the files in the image. Python stays only in the first two checks, which must *execute* Hermes' code; that is probing a Python system, not writing Python.

Add a helper next to `run()`:

```bash
# grep inside the image without running Hermes' interpreter on our behalf.
img_grep() { docker run --rm --entrypoint grep -w /opt/hermes "$IMG" "$@" >/dev/null 2>&1; }
```

Replace the sampling/elicitation check:

```bash
if img_grep -q 'config.get("sampling", {})' tools/mcp_tool_server_run.py \
   && img_grep -q 'sampling_config.get("enabled", True)' tools/mcp_tool_server_run.py; then
  ok "sampling/elicitation still nested dicts defaulting to on"
else
  bad "sampling/elicitation config shape changed -> re-read before trusting the pin"
fi
```

Replace the env-passthrough check:

```bash
if img_grep -q 'required_environment_variables' tools/env_passthrough.py; then
  ok "skill-declared env vars still auto-forwarded (skills.write_approval stays load-bearing)"
else
  bad "skill env passthrough mechanism changed -> revisit skills.write_approval"
fi
```

Replace the approvals-default check:

```bash
out=$(docker run --rm --entrypoint grep -w /opt/hermes "$IMG" -A3 '"approvals"' hermes_cli/config_defaults.py 2>&1 | grep -o '"mode":[[:space:]]*"[a-z]*"' | head -1 | sed 's/.*"\([a-z]*\)"$/\1/')
[ "$out" = "smart" ] && ok "approvals.mode still defaults to 'smart' (we override to manual)" \
                     || note "approvals.mode default is now '$out' -- check our override still makes sense"
```

- [ ] **Step 3: Re-run the verifier against the pinned digest**

```bash
scripts/verify-hermes-image.sh nousresearch/hermes-agent@sha256:1d33f6087fab79d580ffaec34ceed825e11d7bb8e803206be75a56d027c62538
```

Expected: `result: 7 passed, 0 failed`. If a check went red, the bug is more likely in the rewrite than in Hermes — compare against the Python version's output before believing it.

- [ ] **Step 4: Append the decision and commit**

Append to `docs/decisions.md`:

```markdown
---

## 2026-10-22 — M0b is implemented; what the build confirmed and what it changed

**Confirmed.** The evaluation order survives contact with the SDK: `Server.AddTool` takes raw `json.RawMessage` arguments, so one handler shape covers every tool and no tool gets its own path to its handler.

**Changed.** `go.mod` says `go 1.25.0`, not `go 1.24` as the M0b spec stated. `github.com/modelcontextprotocol/go-sdk@v1.8.0` declares `go >= 1.25.0`; the installed 1.24.3 toolchain builds it by auto-downloading 1.25.0.

**Added, not in the spec.** `internal/state`, because `/stop` must survive a restart and `internal/policy` must stay pure — something has to own the file, and it cannot be the pure package. `policy.Request` also gained `Known`, `ValidatorOK` and `CapKey`/`AmountMinor`, without which three of the six rules in the evaluation order could not live inside `Decide`, and the spec is explicit that the order is the security property and is tested there.

**Reopen if:** the trace proxy lands and per-session taint becomes possible, which is the one deferred item that changes `internal/state` rather than adding to it.
```

```bash
git add docs/design.md docs/decisions.md scripts/verify-hermes-image.sh
git commit -m "docs: close the M0b documentation debt

design.md now says why the web toolsets are off rather than just that
they are: the gateway cannot see a tool call it does not serve, so taint
tracking blind to the likeliest injection vector reads as protection
while providing none.

verify-hermes-image.sh greps the image instead of running
inspect.getsource for the three structural checks. Python stays only
where we must execute Hermes' own code."
```

---

## Done when

- [ ] `make test` passes.
- [ ] `make e2e` passes — this is the exit check: `/stop` → `memory.propose_update` → `Deny(killswitch.active)` → an audit record exists → `chhayactl audit verify` passes.
- [ ] `scripts/verify-hermes-image.sh <pinned digest>` reports 7 passed, 0 failed.
- [ ] `docker compose -f deploy/docker-compose.yml up` brings the gateway up, and `/status` on the approver bot answers.
- [ ] `git status --short` shows nothing under `deploy/certs/`.
