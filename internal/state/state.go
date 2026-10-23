// Package state owns the gateway's mutable world: the kill switch, the taint
// flag, and the per-day and per-hour counters. It exists because internal/policy
// must stay pure and something still has to survive a restart.
//
// The file is the source of truth, not any in-memory copy of it. chhayactl is
// a different process from chhayad with its own Store, so a Store that cached
// the file's contents would make `chhayactl stop` invisible to the running
// daemon — and the daemon's next write would flush its stale copy back over
// the operator's kill switch. Every read and every mutation therefore goes to
// disk.
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

// stateVersion is written by every flush and required by every read. It is
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

func fresh() persisted {
	return persisted{V: stateVersion, Spent: map[string]int64{}, Calls: map[string]int{}}
}

// Store is a handle on the state file. It holds no copy of the file's
// contents: mu serialises this process's own read-modify-write cycles, and
// the file itself is what every caller actually reads.
type Store struct {
	mu   sync.Mutex
	path string
}

// Open validates the state file, or accepts that there is none. An unreadable
// file is an error: if we cannot tell whether we are stopped, we are stopped.
// Nothing is cached — Open exists so that a corrupt file stops the gateway at
// startup rather than at the first tool call.
func Open(path string) (*Store, error) {
	if _, err := load(path); err != nil {
		return nil, err
	}
	return &Store{path: path}, nil
}

// load reads and validates the file. A missing file is a legitimate fresh
// start and returns a clean state with no error; anything else that cannot be
// read as state is an error.
func load(path string) (persisted, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fresh(), nil
	}
	if err != nil {
		return persisted{}, fmt.Errorf("read state: %w", err)
	}
	// Decode into a pointer, not a value: json.Unmarshal of the literal `null`
	// into a value target succeeds and leaves it untouched, so a file corrupted
	// down to four bytes would read as "not stopped".
	var p *persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return persisted{}, fmt.Errorf("state file at %s is unreadable, so we cannot tell whether we are stopped: %w", path, err)
	}
	if p == nil {
		return persisted{}, fmt.Errorf("state file at %s holds JSON null, not state; delete it deliberately if you mean to start clean", path)
	}
	if p.V != stateVersion {
		return persisted{}, fmt.Errorf("state file at %s has version %d, want %d; refusing to guess what it means", path, p.V, stateVersion)
	}
	if p.Spent == nil {
		p.Spent = map[string]int64{}
	}
	if p.Calls == nil {
		p.Calls = map[string]int{}
	}
	return *p, nil
}

// Snapshot returns the state as policy.Decide should see it at now, with
// expired counters already zeroed so the caller cannot forget to roll them.
//
// It re-reads the file every time. The file is a few hundred bytes and sits in
// page cache, and correctness is worth that read: this is the only way a
// running daemon sees a kill switch set by another process. There is
// deliberately no mtime or size shortcut — a second write within the same
// timestamp granularity, or one that happens to be the same length, would be
// exactly the write we must not miss.
//
// It returns no error because every caller is on a path where there is nothing
// useful to do with one. A read that fails for any reason other than the file
// not existing therefore fails closed: the returned state has the kill switch
// on. If we cannot tell whether we are stopped, we are stopped.
func (s *Store) Snapshot(now time.Time) policy.State {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := load(s.path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "CRITICAL: cannot read state at %s, so the gateway is treating itself as stopped: %v\n", s.path, err)
		return policy.State{
			KillSwitch: true,
			KilledAt:   now,
			Spent:      map[string]int64{},
			Calls:      map[string]int{},
		}
	}

	st := policy.State{
		KillSwitch:  p.KillSwitch,
		KilledAt:    p.KilledAt,
		Tainted:     p.Tainted,
		TaintedAt:   p.TaintedAt,
		TaintSource: p.TaintSource,
		Spent:       map[string]int64{},
		Calls:       map[string]int{},
	}
	if p.SpendDay == day(now) {
		for k, v := range p.Spent {
			st.Spent[k] = v
		}
	}
	if p.CallHour == hour(now) {
		for k, v := range p.Calls {
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

// update is read-modify-write against the file, not against a cached struct.
// Reading first is what stops this process from clobbering a change another
// one made — most importantly a kill switch set by chhayactl while the daemon
// was running. mu makes the cycle atomic within this process; between
// processes it narrows the window to the few microseconds between the read
// and the rename rather than closing it, which is the right trade for a
// two-writer system where one writer is a human typing a command.
//
// A read that fails is a refused write: we will not overwrite a state file we
// could not understand, because what we cannot read might say we are stopped.
func (s *Store) update(f func(*persisted)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := load(s.path)
	if err != nil {
		return err
	}
	f(&p)
	p.V = stateVersion
	return s.flush(p)
}

// flush writes atomically and durably. Both halves matter: a half-written
// state file is an unreadable one, and an unreadable one stops the gateway.
// os.WriteFile would only reach the page cache, so a crash just after the
// rename could leave the target present with stale bytes — the exact failure
// the temp-file dance is supposed to prevent. Hence the explicit Sync.
func (s *Store) flush(p persisted) error {
	b, err := json.Marshal(p)
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
