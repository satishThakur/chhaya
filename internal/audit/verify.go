package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Verify walks the chain from the genesis record and reports how many records
// it checked. It shares no state with Writer on purpose — chhayactl runs it in
// a different process, so a compromised daemon cannot also be the auditor.
func Verify(path string) (int, error) {
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
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("record %d is not valid JSON: %w", len(out)+1, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
