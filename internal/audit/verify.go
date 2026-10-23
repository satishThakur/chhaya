package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
)

// Verify walks the chain from the genesis record and reports how many records
// it checked. It shares no state with Writer on purpose — chhayactl runs it in
// a different process, so a compromised daemon cannot also be the auditor.
// A missing log is not an intact one. readAll deliberately returns (nil, nil)
// for a missing file because Open's tail() needs that for a fresh start — but
// a caller who asked us to VERIFY a log and has no log must be told so, not
// reassured.
func Verify(path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("no audit log at %s: %w", path, fs.ErrNotExist)
		}
		return 0, err
	}

	recs, err := readAll(path)
	if err != nil {
		return 0, err
	}

	var prev *string
	for i, r := range recs {
		want := uint64(i + 1)
		if r.Seq != want {
			return i, fmt.Errorf("record %d: seq is %d, expected %d — a record was removed or reordered", i+1, r.Seq, want)
		}
		if (prev == nil) != (r.Prev == nil) || (prev != nil && *prev != *r.Prev) {
			return i, fmt.Errorf("record %d (seq %d): prev does not match the previous record's hash", i+1, r.Seq)
		}
		got, err := r.computeHash()
		if err != nil {
			return i, err
		}
		if got != r.Hash {
			return i, fmt.Errorf("record %d (seq %d): content does not match its hash — this record was edited", i+1, r.Seq)
		}
		h := r.Hash
		prev = &h
	}
	return len(recs), nil
}

func readAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a diff can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// DisallowUnknownFields, not json.Unmarshal: Unmarshal drops keys the
		// struct does not declare, so a key injected into a record would be
		// invisible to the hash walk and the chain would verify clean while
		// the bytes on disk said something extra. The same decoder rejects a
		// second JSON value smuggled onto the line after the record.
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var r Record
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("record %d is not a record this log wrote: %w", len(out)+1, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("record %d: the line carries more than one JSON value", len(out)+1)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
