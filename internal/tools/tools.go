// Package tools holds the gateway's tool handlers. In M0b all four are stubs
// or reads: the boundary is exercised before anything valuable sits behind it.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/policy"
	"github.com/satishthakur/chhaya/internal/state"
)

// MemoryNotifier sends a staged proposal to the user's approver. *approval.Bot
// satisfies it; tools does not import approval.
type MemoryNotifier interface {
	ProposeMemory(ctx context.Context, p memory.Proposal) error
}

type Deps struct {
	Version    string
	MemoryRoot string
	State      *state.Store
	Config     policy.Config
	// Notifier posts memory proposals to the approver. Nil fails closed.
	Notifier MemoryNotifier
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
				p, err := memory.Resolve(d.MemoryRoot, str(args["path"]))
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
			Description: "Propose one line for a Personal memory document. This does NOT write the document: " +
				"it sends the line to the user's approver bot, and the gateway appends it only if they tap approve. " +
				"A rationale is required — it is recorded as provenance.",
			Tier: policy.TierInternal, Kind: policy.Write, Trust: mcpsrv.Trusted,
			Redact: []string{"entry"},
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"document path, e.g. taste/food"},` +
				`"entry":{"type":"string","description":"one line, written verbatim to the document after the user approves it, e.g. \"No okra.\""},` +
				`"rationale":{"type":"string","description":"why this change; recorded as provenance"}},` +
				`"required":["path","entry","rationale"]}`),
			Handle: func(ctx context.Context, args map[string]any) (any, error) {
				if d.Notifier == nil {
					return nil, fmt.Errorf("memory proposals are disabled: no approver is wired")
				}
				store := memory.Store{Root: d.MemoryRoot}
				p, err := store.Stage(str(args["path"]), str(args["entry"]), str(args["rationale"]), time.Now())
				if err != nil {
					return nil, err
				}
				if err := d.Notifier.ProposeMemory(ctx, p); err != nil {
					if rmErr := store.Remove(p.ID); rmErr != nil {
						return nil, fmt.Errorf("the approver bot did not receive the proposal: %v; and the staged proposal %s could not be removed: %v", err, p.ID, rmErr)
					}
					return nil, fmt.Errorf("the approver bot did not receive the proposal, so nothing was saved: %v. Tell the user their approver bot is not responding.", err)
				}
				return map[string]any{"proposal_id": p.ID, "status": "sent for approval"}, nil
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

				tools := []map[string]any{}
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

func str(v any) string {
	s, _ := v.(string)
	return s
}
