// Package audit writes a hash-chained, append-only log. Tamper-evidence is the
// point: anyone with the file can tell whether it has been edited, including us.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Record is one line of the log. Field order here is the serialised order, and
// the serialised bytes are what gets hashed, so reordering fields invalidates
// every existing chain. Don't, without a migration.
type Record struct {
	Seq        uint64          `json:"seq"`
	TS         string          `json:"ts"`   // RFC3339Nano, UTC; a string so it round-trips byte for byte
	Prev       *string         `json:"prev"` // null on the genesis record
	Actor      string          `json:"actor"`
	Tool       string          `json:"tool"`
	Kind       string          `json:"kind"`
	Tier       int             `json:"tier"`
	Args       json.RawMessage `json:"args"`
	Decision   string          `json:"decision"`
	Rule       string          `json:"rule"`
	Reason     string          `json:"reason"`
	Taint      Taint           `json:"taint"`
	Approval   *Approval       `json:"approval"`
	Result     Result          `json:"result"`
	DurationMS int64           `json:"duration_ms"`
	Severity   string          `json:"severity,omitempty"` // "critical" when the safety channel itself failed
	Hash       string          `json:"hash"`
}

type Taint struct {
	Tainted bool   `json:"tainted"`
	Source  string `json:"source,omitempty"`
}

type Approval struct {
	Answer    string `json:"answer"` // approved | denied_by_user | timeout
	LatencyMS int64  `json:"latency_ms"`
}

type Result struct {
	Status string `json:"status"` // ok | refused | error
	Error  string `json:"error,omitempty"`
}

// canonicalJSON is the ONE encoding mode this package uses — for hashing, for
// writing, and for verifying. It must be one mode: json.Marshal HTML-escapes
// `<`, `>` and `&`, so mixing it with an unescaped writer would hash different
// bytes than it writes, and the redaction marker `<redacted:NB>` contains two
// of those three characters.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil // Encode appends a newline
}

// MarshalArgs is the supported way to build Record.Args. Use it rather than
// json.Marshal so that the bytes written are the bytes hashed. It cannot fail
// usefully from a caller's point of view, so an unserialisable value becomes a
// sentinel rather than an error nobody can act on.
func MarshalArgs(v any) json.RawMessage {
	b, err := canonicalJSON(v)
	if err != nil {
		return json.RawMessage(`{"_":"args were not serialisable"}`)
	}
	return b
}

// computeHash implements the spec's sha256(canonical_json(record_without_hash) ‖ prev).
// "Without hash" is realised by zeroing the field rather than editing the
// serialised bytes: equally deterministic, and no string surgery to get wrong.
func (r Record) computeHash() (string, error) {
	c := r
	c.Hash = ""
	b, err := canonicalJSON(c)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(b)
	if r.Prev != nil {
		h.Write([]byte(*r.Prev))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
