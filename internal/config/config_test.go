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
		"no timeout":     "caps:\n  food: {per_order_inr: 800, per_day_inr: 1500}\n  rate_limits: {default_per_hour: 60}\nallowlists: {repos: []}\napprovals: {timeout_seconds: 300}\n",
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

func TestNoRepoIsWritableWhicheverWayTheKeyIsWritten(t *testing.T) {
	// Both shapes must mean the same thing: nothing is writable. The
	// representation differs (yaml.v3 gives a non-nil empty slice for the
	// explicit form and nil for the omitted one) and that difference must
	// not matter to anyone.
	explicit := good
	omitted := strings.Replace(good, "  repos: []\n", "", 1)

	for name, body := range map[string]string{"explicit empty": explicit, "key omitted": omitted} {
		t.Run(name, func(t *testing.T) {
			cfg, _, err := Load(write(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Repos) != 0 {
				t.Fatalf("Repos = %v, want nothing writable", cfg.Repos)
			}
		})
	}
}
