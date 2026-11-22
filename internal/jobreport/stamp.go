package jobreport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Stamp is what a job leaves behind when it finishes: when, how it went, and one line of detail.
// The morning report reads these, so a job that stopped running (no stamp, or an old one) is
// found by code rather than by noticing that a message did not arrive. The backup writes the
// same file (Time and Archive), which is why Archive is read here.
type Stamp struct {
	Time    time.Time `json:"time"`
	Detail  string    `json:"detail,omitempty"`
	Failed  string    `json:"failed,omitempty"` // non-empty: the run finished but something in it broke
	Archive string    `json:"archive,omitempty"`
}

// stickyWindow is how long a failed stamp survives a later good one. The dreamer runs once per
// day it covers, back to back; a failure in the first must not be erased by the second.
const stickyWindow = 2 * time.Hour

// WriteStamp records a finished run, atomically.
func WriteStamp(path string, s Stamp) error {
	if prev, err := ReadStamp(path); err == nil && prev.Failed != "" && s.Failed == "" &&
		s.Time.After(prev.Time) && s.Time.Sub(prev.Time) < stickyWindow {
		s.Failed = prev.Failed
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadStamp reads a job's stamp. A missing file is an error: a job that never ran is not a pass.
func ReadStamp(path string) (Stamp, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Stamp{}, err
	}
	var s Stamp
	if err := json.Unmarshal(b, &s); err != nil {
		return Stamp{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.Time.IsZero() {
		return Stamp{}, fmt.Errorf("%s: no time recorded", path)
	}
	if s.Detail == "" {
		s.Detail = s.Archive
	}
	return s, nil
}
