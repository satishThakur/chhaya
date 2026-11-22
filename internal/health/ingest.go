package health

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/jobreport"
	"github.com/satishthakur/chhaya/internal/policy"
)

// Store is where validated rows land.
type Store interface {
	// Upsert writes the rows in one transaction: all of them or none. A row for a day and metric
	// that exists is replaced, so the sidecar re-sending today every few hours is safe.
	Upsert(ctx context.Context, ms []Metric, acts []Activity) error
	RecordRun(ctx context.Context, r Run) error
}

// Run is one pass of the sidecar: what it tried and how it went.
type Run struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"` // ok, partial or failed
	Days       []string  `json:"days,omitempty"`
	Metrics    int       `json:"metrics,omitempty"`
	Activities int       `json:"activities,omitempty"`
	Errors     []string  `json:"errors,omitempty"`
}

// Auditor is the audit log's append.
type Auditor interface {
	Append(audit.Record) (audit.Record, error)
}

const (
	maxBody    = 1 << 20
	minSecret  = 16
	secretHdr  = "X-Ingest-Secret"
	actorSync  = "garmin-sync"
	maxErrors  = 20
	maxErrText = 300
)

// IngestOptions wires the handler. Everything is required except Now.
type IngestOptions struct {
	Store     Store
	Secret    string
	Loc       *time.Location
	Audit     Auditor
	Killed    func() bool // the kill switch: while it is on, nothing is written
	StampPath string      // where the last run is recorded for the morning report; empty = not recorded
	Now       func() time.Time
}

// Ingest is the localhost listener the sync sidecar posts to. It is not an MCP tool and Hermes
// cannot call it: it needs the shared secret, which only the sidecar holds. Every request it
// accepts or refuses leaves an audit record.
type Ingest struct {
	o       IngestOptions
	secretH [32]byte
}

// NewIngest refuses to build a handler with no real secret: an open write door is worse than none.
func NewIngest(o IngestOptions) (*Ingest, error) {
	switch {
	case len(o.Secret) < minSecret:
		return nil, fmt.Errorf("the ingest secret must be at least %d characters", minSecret)
	case o.Store == nil || o.Audit == nil || o.Killed == nil || o.Loc == nil:
		return nil, errors.New("ingest needs a store, an audit log, a kill switch and a time zone")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Ingest{o: o, secretH: sha256.Sum256([]byte(o.Secret))}, nil
}

func (h *Ingest) authorised(r *http.Request) bool {
	got := sha256.Sum256([]byte(r.Header.Get(secretHdr)))
	return subtle.ConstantTimeCompare(got[:], h.secretH[:]) == 1
}

func (h *Ingest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ready" && r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"ok": true})
		return
	}
	var tool string
	switch r.URL.Path {
	case "/ingest/day":
		tool = "health.ingest_day"
	case "/ingest/run":
		tool = "health.ingest_run"
	default:
		reply(w, 404, map[string]any{"error": "not found"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		reply(w, 405, map[string]any{"error": "POST only"})
		return
	}
	if !h.authorised(r) {
		h.record(tool, nil, "deny", "ingest.bad_secret", "the shared secret was missing or wrong", audit.Result{Status: "refused"})
		reply(w, 401, map[string]any{"error": "unauthorised"})
		return
	}
	if h.o.Killed() {
		h.record(tool, nil, "deny", "ingest.killswitch", "the kill switch is on", audit.Result{Status: "refused"})
		reply(w, 503, map[string]any{"error": "the gateway is stopped"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if tool == "health.ingest_day" {
		h.day(w, r, tool)
	} else {
		h.run(w, r, tool)
	}
}

func (h *Ingest) day(w http.ResponseWriter, r *http.Request, tool string) {
	d, err := Decode(r.Body)
	var ms []Metric
	var acts []Activity
	if err == nil {
		ms, acts, err = d.Rows(h.o.Loc, h.o.Now())
	}
	if err != nil {
		code := 400
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			code = 413
		}
		h.record(tool, map[string]any{"date": d.Date}, "deny", "ingest.invalid", err.Error(), audit.Result{Status: "refused"})
		reply(w, code, map[string]any{"error": err.Error()})
		return
	}
	args := map[string]any{"date": d.Date, "metrics": len(ms), "activities": len(acts)}
	if err := h.o.Store.Upsert(r.Context(), ms, acts); err != nil {
		h.record(tool, args, "allow", "ingest.ok", "accepted", audit.Result{Status: "error", Error: err.Error()})
		reply(w, 502, map[string]any{"error": "the store refused the day"})
		return
	}
	if !h.record(tool, args, "allow", "ingest.ok", "accepted", audit.Result{Status: "ok"}) {
		reply(w, 500, map[string]any{"error": "the audit log is not writable"})
		return
	}
	reply(w, 200, map[string]any{"metrics": len(ms), "activities": len(acts)})
}

var runStatuses = map[string]bool{"ok": true, "partial": true, "failed": true}

func (h *Ingest) run(w http.ResponseWriter, r *http.Request, tool string) {
	var run Run
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&run)
	switch {
	case err != nil:
	case !runStatuses[run.Status]:
		err = fmt.Errorf("status %q is not ok, partial or failed", run.Status)
	case run.StartedAt.IsZero() || run.FinishedAt.IsZero() || run.FinishedAt.Before(run.StartedAt):
		err = errors.New("started_at and finished_at are required, in order")
	case run.FinishedAt.After(h.o.Now().Add(time.Hour)):
		err = errors.New("finished_at is in the future")
	case run.Metrics < 0 || run.Activities < 0 || len(run.Days) > 31:
		err = errors.New("counts out of range")
	}
	if err != nil {
		h.record(tool, nil, "deny", "ingest.invalid", err.Error(), audit.Result{Status: "refused"})
		reply(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if len(run.Errors) > maxErrors {
		run.Errors = run.Errors[:maxErrors]
	}
	for i, e := range run.Errors {
		if len(e) > maxErrText {
			run.Errors[i] = e[:maxErrText]
		}
	}
	args := map[string]any{"status": run.Status, "days": len(run.Days), "metrics": run.Metrics, "activities": run.Activities, "errors": len(run.Errors)}
	if err := h.o.Store.RecordRun(r.Context(), run); err != nil {
		h.record(tool, args, "allow", "ingest.ok", "accepted", audit.Result{Status: "error", Error: err.Error()})
		reply(w, 502, map[string]any{"error": "the store refused the run"})
		return
	}
	if h.o.StampPath != "" {
		st := jobreport.Stamp{Time: run.FinishedAt, Detail: fmt.Sprintf("%d metrics, %d activities over %d day(s)", run.Metrics, run.Activities, len(run.Days))}
		if run.Status != "ok" {
			st.Failed = run.Status
			if len(run.Errors) > 0 {
				st.Failed += ": " + strings.Join(run.Errors, "; ")
			}
		}
		if err := jobreport.WriteStamp(h.o.StampPath, st); err != nil {
			h.record(tool, args, "allow", "ingest.ok", "accepted", audit.Result{Status: "error", Error: "stamp: " + err.Error()})
			reply(w, 500, map[string]any{"error": "could not record the run"})
			return
		}
	}
	if !h.record(tool, args, "allow", "ingest.ok", "accepted", audit.Result{Status: "ok"}) {
		reply(w, 500, map[string]any{"error": "the audit log is not writable"})
		return
	}
	reply(w, 200, map[string]any{"recorded": true})
}

// record appends one audit record and reports whether it landed.
func (h *Ingest) record(tool string, args map[string]any, decision, rule, reason string, res audit.Result) bool {
	if args == nil {
		args = map[string]any{}
	}
	_, err := h.o.Audit.Append(audit.Record{
		Actor: actorSync, Tool: tool, Kind: policy.Write.String(), Tier: int(policy.TierInternal),
		Args: audit.MarshalArgs(args), Decision: decision, Rule: rule, Reason: reason, Result: res,
	})
	return err == nil
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
