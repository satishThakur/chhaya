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
