package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/jobreport"
)

type fakeStore struct {
	metrics []Metric
	acts    []Activity
	runs    []Run
	err     error
}

func (f *fakeStore) Upsert(_ context.Context, ms []Metric, as []Activity) error {
	if f.err != nil {
		return f.err
	}
	f.metrics = append(f.metrics, ms...)
	f.acts = append(f.acts, as...)
	return nil
}
func (f *fakeStore) RecordRun(_ context.Context, r Run) error {
	if f.err != nil {
		return f.err
	}
	f.runs = append(f.runs, r)
	return nil
}

type fakeAudit struct {
	got []audit.Record
	err error
}

func (a *fakeAudit) Append(r audit.Record) (audit.Record, error) {
	if a.err != nil {
		return audit.Record{}, a.err
	}
	a.got = append(a.got, r)
	return r, nil
}

type rig struct {
	h     *Ingest
	store *fakeStore
	aud   *fakeAudit
	stamp string
	kill  bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{store: &fakeStore{}, aud: &fakeAudit{}, stamp: filepath.Join(t.TempDir(), "health-sync.json")}
	h, err := NewIngest(IngestOptions{Store: r.store, Secret: "s3cret-s3cret-s3cret", Loc: ist, Audit: r.aud,
		Killed: func() bool { return r.kill }, StampPath: r.stamp, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	return r
}

func (r *rig) post(path, secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Ingest-Secret", secret)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

const secret = "s3cret-s3cret-s3cret"

func TestNoSecretConfiguredMeansNoIngest(t *testing.T) {
	for _, s := range []string{"", "short"} {
		if _, err := NewIngest(IngestOptions{Store: &fakeStore{}, Secret: s, Loc: ist, Audit: &fakeAudit{}, Killed: func() bool { return false }}); err == nil {
			t.Errorf("secret %q: the listener must not start without a real secret", s)
		}
	}
}

func TestAGoodDayIsStoredAndAuditedWithoutItsValues(t *testing.T) {
	r := newRig(t)
	w := r.post("/ingest/day", secret, fullDay)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(r.store.metrics) != 26 || len(r.store.acts) != 1 {
		t.Fatalf("stored %d metrics %d activities", len(r.store.metrics), len(r.store.acts))
	}
	if len(r.aud.got) != 1 {
		t.Fatalf("audit records: %d", len(r.aud.got))
	}
	a := r.aud.got[0]
	if a.Actor != "garmin-sync" || a.Tool != "health.ingest_day" || a.Decision != "allow" || a.Kind != "write" || a.Result.Status != "ok" {
		t.Fatalf("record: %+v", a)
	}
	if !strings.Contains(string(a.Args), "2026-11-21") || strings.Contains(string(a.Args), "9120") || strings.Contains(string(a.Args), "27000") {
		t.Fatalf("args must name the day and count rows, never carry the values: %s", a.Args)
	}
}

func TestAWrongOrMissingSecretStoresNothingAndIsRecorded(t *testing.T) {
	for name, sec := range map[string]string{"missing": "", "wrong": "nope-nope-nope-nope", "prefix": "s3cret"} {
		r := newRig(t)
		if w := r.post("/ingest/day", sec, fullDay); w.Code != 401 {
			t.Errorf("%s: %d", name, w.Code)
		}
		if len(r.store.metrics)+len(r.store.acts) != 0 {
			t.Errorf("%s: stored something", name)
		}
		if len(r.aud.got) != 1 || r.aud.got[0].Decision != "deny" || r.aud.got[0].Rule != "ingest.bad_secret" {
			t.Errorf("%s: audit %+v", name, r.aud.got)
		}
		if strings.Contains(string(r.aud.got[0].Args)+r.aud.got[0].Reason, sec+"x") {
			t.Errorf("%s: the audit record must not echo the secret", name)
		}
	}
}

func TestOnlyPostToTheTwoRoutes(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path string }{{"GET", "/ingest/day"}, {"PUT", "/ingest/day"}, {"POST", "/ingest/other"}, {"POST", "/"}, {"DELETE", "/ingest/run"}} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		req.Header.Set("X-Ingest-Secret", secret)
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		if w.Code != 404 && w.Code != 405 {
			t.Errorf("%s %s: %d", c.method, c.path, w.Code)
		}
	}
	if len(r.store.metrics) != 0 {
		t.Fatal("stored something")
	}
}

func TestABadDayIsRefusedWithItsReasonAndStoresNothing(t *testing.T) {
	r := newRig(t)
	w := r.post("/ingest/day", secret, `{"date":"2026-11-21","stress":{"avg":150}}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "stress_avg") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(r.store.metrics) != 0 || len(r.aud.got) != 1 || r.aud.got[0].Decision != "deny" || r.aud.got[0].Rule != "ingest.invalid" {
		t.Fatalf("stored %d, audit %+v", len(r.store.metrics), r.aud.got)
	}
	if w := r.post("/ingest/day", secret, `{"date":"2026-11-21","hrv":{"average_hrv":5}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "unknown field") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	r := newRig(t)
	big := `{"date":"2026-11-21","sleep":{"total_seconds":1},"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	if w := r.post("/ingest/day", secret, big); w.Code != 413 && w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
	if len(r.store.metrics) != 0 {
		t.Fatal("stored something")
	}
}

func TestTheKillSwitchRefusesIngest(t *testing.T) {
	r := newRig(t)
	r.kill = true
	if w := r.post("/ingest/day", secret, fullDay); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
	if len(r.store.metrics) != 0 || len(r.aud.got) != 1 || r.aud.got[0].Rule != "ingest.killswitch" {
		t.Fatalf("stored %d, audit %+v", len(r.store.metrics), r.aud.got)
	}
}

func TestAStoreFailureIsAnErrorTheSidecarSees(t *testing.T) {
	r := newRig(t)
	r.store.err = errors.New("db down")
	w := r.post("/ingest/day", secret, fullDay)
	if w.Code != 502 || strings.Contains(w.Body.String(), "db down") {
		t.Fatalf("%d %s (the cause belongs in the audit log, not the response)", w.Code, w.Body)
	}
	if len(r.aud.got) != 1 || r.aud.got[0].Result.Status != "error" {
		t.Fatalf("audit %+v", r.aud.got)
	}
}

func TestABrokenAuditLogRefusesTheWrite(t *testing.T) {
	r := newRig(t)
	r.aud.err = errors.New("disk full")
	if w := r.post("/ingest/day", secret, fullDay); w.Code != 500 {
		t.Fatalf("%d: a write with no record must not look like a success", w.Code)
	}
}

func TestARunRecordUpdatesTheStampTheMorningReportReads(t *testing.T) {
	r := newRig(t)
	ok := `{"started_at":"2026-11-22T05:00:00Z","finished_at":"2026-11-22T05:02:00Z","status":"ok","days":["2026-11-21","2026-11-22"],"metrics":40,"activities":1}`
	if w := r.post("/ingest/run", secret, ok); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	st, err := jobreport.ReadStamp(r.stamp)
	if err != nil || st.Failed != "" || !st.Time.Equal(time.Date(2026, 11, 22, 5, 2, 0, 0, time.UTC)) || !strings.Contains(st.Detail, "40 metrics") {
		t.Fatalf("%+v %v", st, err)
	}
	bad := `{"started_at":"2026-11-22T06:00:00Z","finished_at":"2026-11-22T06:00:05Z","status":"failed","days":["2026-11-22"],"errors":["login: 401"]}`
	if w := r.post("/ingest/run", secret, bad); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	st, _ = jobreport.ReadStamp(r.stamp)
	if st.Failed == "" || !strings.Contains(st.Failed, "login: 401") {
		t.Fatalf("a failed run must be recorded as failed: %+v", st)
	}
	if len(r.store.runs) != 2 {
		t.Fatalf("runs kept: %d", len(r.store.runs))
	}
	if _, err := os.Stat(r.stamp); err != nil {
		t.Fatal(err)
	}
}

func TestARunRecordWithAnUnknownStatusIsRefused(t *testing.T) {
	r := newRig(t)
	if w := r.post("/ingest/run", secret, `{"started_at":"2026-11-22T05:00:00Z","finished_at":"2026-11-22T05:02:00Z","status":"great"}`); w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
	if len(r.store.runs) != 0 {
		t.Fatal("stored it")
	}
}

func TestReadyNeedsNoSecretAndHoldsNoData(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "steps") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
