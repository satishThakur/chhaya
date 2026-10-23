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

// memoryPath resolves a document path inside the memory root and refuses
// anything that leaves it, including a path whose final component does not
// exist yet — the case a write would hit.
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

	// Reject traversal on the cleaned path BEFORE appending ".md". Clean turns
	// "taste/food/../../.." into "..", and ".."+".md" fuses into the ordinary
	// filename "...md" — which lands inside the root and silently looks safe.
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}

	p := filepath.Join(absRoot, clean+".md")

	// The parent must exist and resolve inside the root. Resolving only the
	// full path skips the check whenever the final component does not exist
	// yet, which is exactly the case a write would hit.
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", fmt.Errorf("path %q: %w", rel, err)
	}
	if !containedIn(absRoot, parent) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}

	final := filepath.Join(parent, filepath.Base(p))

	// If the final component already exists it may itself be a symlink out.
	if resolved, err := filepath.EvalSymlinks(final); err == nil && !containedIn(absRoot, resolved) {
		return "", fmt.Errorf("path %q is outside the memory root", rel)
	}
	return final, nil
}

func containedIn(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
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
