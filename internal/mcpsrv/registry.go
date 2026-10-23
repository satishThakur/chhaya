package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

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
// except Validate, CapKey and Redact; New refuses anything incomplete, because
// CLAUDE.md says a tool with no tier is a bug, not a default.
type Registration struct {
	Name        string
	Description string // states the tool's limits, so the agent plans well
	Tier        policy.Tier
	Kind        policy.Kind
	Trust       Trust
	Schema      json.RawMessage
	Validate    Validator // consulted whenever supplied; required for tier 1
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
		case r.Tier == policy.TierMoney:
			return nil, fmt.Errorf("%s: tier 2 is not registerable yet — per-order and daily caps are not wired "+
				"into Invoke (policy.Request.AmountMinor is never set and state.RecordSpend is never called), so "+
				"a money tool would be protected by the approval card alone. Wire the caps first", r.Name)
		case r.Tier == policy.TierReversible && r.Validate == nil:
			return nil, fmt.Errorf("%s: a tier-1 tool must supply a validator", r.Name)
		}
		if _, dup := out[r.Name]; dup {
			return nil, fmt.Errorf("%s: registered twice", r.Name)
		}
		// Clone the mutable fields on the way in: the caller of New keeps its
		// own reference to the slice or byte slice it built the registration
		// from, and a mutation through that reference after registration must
		// not be able to reach what the gateway reads on every later call.
		r.Schema = bytes.Clone(r.Schema)
		r.Redact = slices.Clone(r.Redact)
		out[r.Name] = r
	}
	return out, nil
}
