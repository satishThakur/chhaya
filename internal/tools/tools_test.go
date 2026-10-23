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

func TestCapabilitiesClampsCapsToZero(t *testing.T) {
	d := deps(t)
	// Spend more than the daily cap
	d.State.RecordSpend("food", 200000, time.Now())

	out, err := find(t, All(d), "capabilities").Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	caps := m["caps"].(map[string]any)["food"].(map[string]any)
	if caps["remaining_today_inr"] != int64(0) {
		t.Errorf("remaining = %v, want 0 (clamped); never negative", caps["remaining_today_inr"])
	}
}

func TestMemoryGetCannotBeEscapedBySymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "taste"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "taste", "food.md"), []byte("# Food\n"), 0o600)
	os.WriteFile(filepath.Join(outside, "loot.md"), []byte("TOP-SECRET"), 0o600)

	// A link to a file outside, and a link to a directory outside.
	if err := os.Symlink(filepath.Join(outside, "loot.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"link",                // final component is a symlink out, target exists
		"escape/loot",         // parent is a symlink out, target exists
		"escape/nope",         // parent is a symlink out, target ABSENT
		"taste/food/../../..", // Clean fuses this into a filename; must still refuse
		".",
	} {
		t.Run(rel, func(t *testing.T) {
			if p, err := memoryPath(root, rel); err == nil {
				t.Fatalf("%q escaped the memory root, resolved to %q", rel, p)
			}
		})
	}

	// A legitimate path that happens to contain traversal must still work.
	if _, err := memoryPath(root, "taste/../taste/food"); err != nil {
		t.Fatalf("a path that stays inside the root was refused: %v", err)
	}
}

func TestProposeUpdateRefusesOutOfRootPath(t *testing.T) {
	d := deps(t)
	h := find(t, All(d), "memory.propose_update").Handle
	if _, err := h(context.Background(), map[string]any{
		"path": "../../../etc/passwd", "diff": "+x", "rationale": "nope",
	}); err == nil {
		t.Fatal("propose_update must refuse out-of-root paths just like memory.get")
	}
}
