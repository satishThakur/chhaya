package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Runner runs an external command (the aws CLI) and returns its stdout.
type Runner func(name string, args ...string) ([]byte, error)

type ExportOptions struct {
	Home   string        // ~/.chhaya: hermes/state.db and gateway/audit.jsonl are read from here
	OutDir string        // traces are written to OutDir/days/DAY/trace-ID.json
	S3     string        // optional s3://bucket/evals prefix to upload to
	Since  time.Duration // sessions with a message newer than Now-Since; 0 means all
	Now    time.Time
	Query  Query
	Run    Runner
}

type ExportResult struct {
	Written  []string // local paths
	Uploaded int
	Flagged  int // traces with gateway calls that no message claimed
}

func (r ExportResult) String() string {
	return fmt.Sprintf("exported %d trace(s), uploaded %d, %d with unmatched gateway calls", len(r.Written), r.Uploaded, r.Flagged)
}

// Export builds a trace for each recent session and writes it. Reading state.db and the
// audit log changes nothing there. A failed upload is reported after every trace has been
// tried, so one bad object does not hide the rest.
func Export(o ExportOptions) (ExportResult, error) {
	var res ExportResult
	if o.Query == nil {
		o.Query = SQLiteQuery
	}
	var since time.Time
	if o.Since > 0 {
		since = o.Now.Add(-o.Since)
	}
	entries, err := ReadAudit(filepath.Join(o.Home, "gateway", "audit.jsonl"))
	if err != nil {
		return res, err
	}
	traces, err := ReadSessions(filepath.Join(o.Home, "hermes", "state.db"), since, o.Query)
	if err != nil {
		return res, err
	}
	m := NewMatcher(entries)
	for _, t := range traces {
		m.Attach(t.Messages)
	}
	var errs []error
	for _, t := range traces {
		t.Unmatched, t.Events = m.Residue(t.Started, t.LastMessage)
		t.Counters = Count(t.Messages)
		t.MessageCount = len(t.Messages)
		t.LLM = ReadLLM(filepath.Join(o.Home, "hermes", "sessions"), t)
		t.ExportedAt = o.Now.UTC()
		if len(t.Unmatched) > 0 {
			res.Flagged++
		}
		rel := filepath.Join("days", t.Day, "trace-"+t.ID+".json")
		dst := filepath.Join(o.OutDir, rel)
		b, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := writeAtomic(dst, b); err != nil {
			errs = append(errs, err)
			continue
		}
		res.Written = append(res.Written, dst)
		if o.S3 != "" {
			key := strings.TrimRight(o.S3, "/") + "/" + filepath.ToSlash(rel)
			if _, err := o.Run("aws", "s3", "cp", "--only-show-errors", dst, key); err != nil {
				errs = append(errs, fmt.Errorf("upload %s: %w", t.ID, err))
				continue
			}
			res.Uploaded++
		}
	}
	return res, errors.Join(errs...)
}

// writeAtomic writes via a uniquely named temp file in the same directory, then renames,
// so a reader never sees half a file and two writers never share a temp name.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
