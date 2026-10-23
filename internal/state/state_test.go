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

// TestAStopFromAnotherProcessIsSeenAndSurvives is the probe for the bug where
// a Store held the file's contents in memory from Open onward. chhayactl is a
// different process with a different Store, so its /stop was invisible to the
// running daemon, and the daemon's next write flushed its stale snapshot back
// over the operator's kill switch. Both halves are asserted here.
func TestAStopFromAnotherProcessIsSeenAndSurvives(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")

	daemon, err := Open(p) // the long-lived daemon Store
	if err != nil {
		t.Fatal(err)
	}
	daemon.RecordCall("probe.write", time.Now())

	cli, err := Open(p) // a separate process
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.SetKillSwitch(true, time.Now()); err != nil {
		t.Fatal(err)
	}

	if !daemon.Snapshot(time.Now()).KillSwitch {
		t.Fatal("a running daemon does not see chhayactl stop")
	}
	daemon.RecordCall("probe.write", time.Now()) // the daemon's next write
	again, _ := Open(p)
	if !again.Snapshot(time.Now()).KillSwitch {
		t.Fatal("the daemon's next state write erased the operator's kill switch")
	}
}

// TestAnUnreadableFileFailsClosedOnEveryRead covers the state file going bad
// after Open has already succeeded — the window Open itself cannot cover.
// Snapshot has no error to return, so the failure has to show up as the kill
// switch being on, and a mutation must refuse rather than overwrite a file it
// could not read.
func TestAnUnreadableFileFailsClosedOnEveryRead(t *testing.T) {
	p := tmp(t)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCall("ping", t0); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot(t0).KillSwitch {
		t.Fatal("precondition: a readable file should not read as stopped")
	}

	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Snapshot(t0)
	if !got.KillSwitch {
		t.Fatal("a state file we cannot read must read as stopped")
	}
	if err := s.RecordCall("ping", t0); err == nil {
		t.Fatal("a mutation must refuse rather than overwrite state it could not read")
	}
	// And the unreadable bytes are still there: the refused mutation did not
	// quietly replace them with a clean file.
	if b, _ := os.ReadFile(p); string(b) != "{not json" {
		t.Fatalf("the refused write changed the file: %s", b)
	}
}

// TestADeletedFileIsAFreshStartNotAStop pins the one read failure that must
// NOT fail closed. Provisioning a new box, or a deliberate reset, leaves no
// file; treating that as "stopped" would mean a gateway that never starts.
func TestADeletedFileIsAFreshStartNotAStop(t *testing.T) {
	p := tmp(t)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKillSwitch(true, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot(t0).KillSwitch {
		t.Fatal("a missing file is a fresh start, not a stop")
	}
}
