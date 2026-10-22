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
