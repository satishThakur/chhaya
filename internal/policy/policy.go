// Package policy decides whether a tool call is allowed. It is pure: no I/O,
// no ambient clock, no network. The clock is a parameter rather than an ambient
// call so that Decide stays pure and every safety property is reproducible in
// a table test; no rule reads it yet. Every safety property is therefore a table
// test with no mocks.
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

	// ArgsOK reports whether the caller's arguments parsed into an object at
	// all. False denies: a request we cannot read is one we cannot evaluate.
	ArgsOK    bool
	ArgsError string

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
	// Kind is carried through from the registration, alongside Tier, so that
	// anything writing an audit record about this decision — including
	// internal/approval, which never sees the Request — states the call's
	// real kind instead of assuming one.
	Kind   Kind
	RuleID string
	Reason string
	Redact []string
}
