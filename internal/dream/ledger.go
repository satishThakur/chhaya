package dream

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Ledger remembers which conversations the dreamer has read, by trace id and the time of its last
// message, so a night that fails halfway can be rerun and a resumed conversation (a later last
// message) is read again. It lives on the box, outside the memory repo.
type Ledger struct {
	path string
	Done map[string]string `json:"done"`
}

func OpenLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path, Done: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("%s is not a ledger: %w", path, err)
	}
	if l.Done == nil {
		l.Done = map[string]string{}
	}
	return l, nil
}

func (l *Ledger) Seen(id, last string) bool { return l.Done[id] == last }

// Mark records a conversation as read, atomically.
func (l *Ledger) Mark(id, last string) error {
	l.Done[id] = last
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
