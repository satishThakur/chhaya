package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Writer appends to the log. One per process; Append is safe for concurrent use.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	lastSeq  uint64
	lastHash string
}

// Open opens the log for appending, verifying the existing chain first.
// A broken chain is a refusal, not a warning: if we cannot trust what the log
// already says, appending to it only makes the lie longer.
func Open(path string) (*Writer, error) {
	if _, err := os.Stat(path); err == nil {
		if _, err := Verify(path); err != nil {
			return nil, fmt.Errorf("refusing to append to a log that does not verify: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	seq, hash, err := tail(path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, lastSeq: seq, lastHash: hash}, nil
}

// Append completes r with its sequence, previous hash and own hash, writes it
// as one line, and fsyncs. It returns the completed record so the caller can
// log or assert on it.
func (w *Writer) Append(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	r.Seq = w.lastSeq + 1
	if r.TS == "" {
		r.TS = nowStamp()
	}
	if w.lastHash == "" {
		r.Prev = nil
	} else {
		prev := w.lastHash
		r.Prev = &prev
	}
	if r.Args == nil {
		r.Args = json.RawMessage("{}")
	}

	h, err := r.computeHash()
	if err != nil {
		return Record{}, err
	}
	r.Hash = h

	line, err := canonicalJSON(r)
	if err != nil {
		return Record{}, err
	}
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		return Record{}, err
	}
	// fsync on every record. The log is small and the thing it protects
	// against includes "the box lost power mid-incident".
	if err := w.f.Sync(); err != nil {
		return Record{}, err
	}

	w.lastSeq, w.lastHash = r.Seq, r.Hash
	return r, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func tail(path string) (uint64, string, error) {
	recs, err := readAll(path)
	if err != nil {
		return 0, "", err
	}
	if len(recs) == 0 {
		return 0, "", nil
	}
	last := recs[len(recs)-1]
	return last.Seq, last.Hash, nil
}
