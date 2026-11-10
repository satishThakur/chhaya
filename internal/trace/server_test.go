package trace

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const host = "127.0.0.1:8787"

func newStore(t *testing.T) (*Store, *[]call) {
	t.Helper()
	var calls []call
	s := &Store{Dir: t.TempDir(), Run: fakeRun(&calls, ""), Now: func() time.Time { return time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC) }}
	return s, &calls
}

func putTrace(t *testing.T, s *Store, day, id string) {
	t.Helper()
	must(t, writeAtomic(s.local(day, "trace", id), []byte(`{"id":"`+id+`","day":"`+day+`","messages":[]}`)))
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var write = map[string]string{"X-Chhaya": "1", "Content-Type": "application/json"}

func TestHandlerRefusesAForeignHost(t *testing.T) {
	s, _ := newStore(t)
	putTrace(t, s, "2026-11-09", "s1")
	h := NewHandler(s, []string{host})
	for _, bad := range []string{"evil.example:8787", "127.0.0.1:9999", "", "127.0.0.1"} {
		r := httptest.NewRequest("GET", "/api/index", nil)
		r.Host = bad
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q got %d; DNS rebinding would read private traces", bad, w.Code)
		}
	}
	if w := do(h, "GET", "/api/index", "", nil); w.Code != 200 {
		t.Fatalf("own host refused: %d", w.Code)
	}
}

func TestWritesNeedTheCustomHeader(t *testing.T) {
	s, calls := newStore(t)
	h := NewHandler(s, []string{host})
	body := `{"pass":false,"note":"n"}`
	if w := do(h, "PUT", "/api/annotation/2026-11-09/s1", body, map[string]string{"Content-Type": "application/json"}); w.Code != http.StatusForbidden {
		t.Errorf("PUT without header = %d; a cross-site form post could write", w.Code)
	}
	if w := do(h, "POST", "/api/sync", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("sync without header = %d", w.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("aws was called: %v", *calls)
	}
	if _, err := os.Stat(s.local("2026-11-09", "open-code", "s1")); err == nil {
		t.Error("annotation was written without the header")
	}
}

func TestPathsThatEscapeAreRefused(t *testing.T) {
	s, calls := newStore(t)
	h := NewHandler(s, []string{host})
	secret := filepath.Join(filepath.Dir(s.Dir), "secret.json")
	must(t, os.WriteFile(secret, []byte(`{"secret":true}`), 0o600))
	for _, p := range []string{
		"/api/trace/..%2f..%2fsecret/x", "/api/trace/2026-11-09/..%2f..%2f..%2fsecret",
		"/api/trace/2026-11-09/a%2fb", "/api/trace/20261109/s1", "/api/trace/2026-11-09/x..y%2f..",
		"/api/annotation/2026-11-09/..", "/api/trace/2026-11-09/s1%00",
	} {
		w := do(h, "GET", p, "", nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("%s -> %d %s", p, w.Code, w.Body.String())
		}
	}
	if w := do(h, "PUT", "/api/annotation/2026-11-09/..%2f..%2fx", `{}`, write); w.Code == 200 {
		t.Errorf("write with a bad id succeeded")
	}
	if len(*calls) != 0 {
		t.Errorf("a bad reference reached the aws CLI: %v", *calls)
	}
}

func TestAnnotationRoundTripAndUnsyncedCount(t *testing.T) {
	s, _ := newStore(t)
	putTrace(t, s, "2026-11-09", "s1")
	putTrace(t, s, "2026-11-09", "s2")
	h := NewHandler(s, []string{host})

	if w := do(h, "GET", "/api/annotation/2026-11-09/s1", "", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "null" {
		t.Fatalf("no annotation yet should be null: %d %q", w.Code, w.Body.String())
	}
	w := do(h, "PUT", "/api/annotation/2026-11-09/s1", `{"pass":false,"note":"asked my address","wanted":"check memory","mode":"didnt-look","message_count":9}`, write)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unsynced":1`) {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	var a Annotation
	w = do(h, "GET", "/api/annotation/2026-11-09/s1", "", nil)
	must(t, json.Unmarshal(w.Body.Bytes(), &a))
	if a.Pass == nil || *a.Pass || a.Note != "asked my address" || a.TraceID != "s1" || a.Day != "2026-11-09" || a.MessageCount != 9 {
		t.Errorf("round trip: %+v", a)
	}
	if !a.UpdatedAt.Equal(time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("UpdatedAt = %v", a.UpdatedAt)
	}
	var idx struct {
		Refs     []Ref `json:"refs"`
		Unsynced int   `json:"unsynced"`
	}
	must(t, json.Unmarshal(do(h, "GET", "/api/index", "", nil).Body.Bytes(), &idx))
	if len(idx.Refs) != 2 || !idx.Refs[0].Annotated || idx.Refs[1].Annotated || idx.Unsynced != 1 {
		t.Errorf("index: %+v", idx)
	}
}

func TestPutCannotSetTraceIDOrDayFromTheBody(t *testing.T) {
	s, _ := newStore(t)
	h := NewHandler(s, []string{host})
	do(h, "PUT", "/api/annotation/2026-11-09/s1", `{"trace_id":"../../x","day":"1999-01-01","note":"n"}`, write)
	b, err := os.ReadFile(s.local("2026-11-09", "open-code", "s1"))
	must(t, err)
	var a Annotation
	must(t, json.Unmarshal(b, &a))
	if a.TraceID != "s1" || a.Day != "2026-11-09" {
		t.Fatalf("body overrode the path: %+v", a)
	}
}

func TestPutRejectsUnknownFieldsAndHugeBodies(t *testing.T) {
	s, _ := newStore(t)
	h := NewHandler(s, []string{host})
	if w := do(h, "PUT", "/api/annotation/2026-11-09/s1", `{"note":"n","evil":1}`, write); w.Code != 400 {
		t.Errorf("unknown field = %d", w.Code)
	}
	if w := do(h, "PUT", "/api/annotation/2026-11-09/s1", `{"note":"`+strings.Repeat("a", maxAnnotationField+1)+`"}`, write); w.Code != 400 {
		t.Errorf("oversize note = %d", w.Code)
	}
}

func TestSyncPushesOnlyChangedAnnotationsAndKeepsFailuresUnsynced(t *testing.T) {
	var calls []call
	failing := ""
	s := &Store{Dir: t.TempDir(), S3: "s3://bkt/evals", Now: time.Now}
	s.Run = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, args})
		if failing != "" && strings.Contains(strings.Join(args, " "), failing) {
			return nil, errors.New("denied")
		}
		return nil, nil
	}
	must(t, s.SaveAnnotation("2026-11-09", "s1", Annotation{Note: "one"}))
	must(t, s.SaveAnnotation("2026-11-09", "s2", Annotation{Note: "two"}))

	failing = "open-code-s2"
	n, err := s.Sync()
	if err == nil || n != 1 {
		t.Fatalf("n=%d err=%v; s1 should push and s2's failure be reported", n, err)
	}
	un, _ := s.Unsynced()
	if len(un) != 1 || un[0].ID != "s2" {
		t.Fatalf("unsynced = %+v; the failed one must stay counted", un)
	}

	failing, calls = "", nil
	n, err = s.Sync()
	if err != nil || n != 1 || len(calls) != 1 || !strings.HasSuffix(calls[0].args[len(calls[0].args)-1], "/days/2026-11-09/open-code-s2.json") {
		t.Fatalf("second sync should push only s2: n=%d err=%v calls=%v", n, err, calls)
	}

	calls = nil
	if n, _ := s.Sync(); n != 0 || len(calls) != 0 {
		t.Fatalf("nothing changed, nothing should upload: n=%d calls=%v", n, calls)
	}
	must(t, s.SaveAnnotation("2026-11-09", "s1", Annotation{Note: "one, edited"}))
	if un, _ := s.Unsynced(); len(un) != 1 || un[0].ID != "s1" {
		t.Fatalf("an edited annotation must become unsynced again: %+v", un)
	}
}

func TestSyncWithoutABucketSaysSo(t *testing.T) {
	s, _ := newStore(t)
	must(t, s.SaveAnnotation("2026-11-09", "s1", Annotation{}))
	if _, err := s.Sync(); err == nil {
		t.Fatal("sync with no bucket must error, not report success")
	}
	h := NewHandler(s, []string{host})
	if w := do(h, "POST", "/api/sync", "", write); w.Code == 200 {
		t.Fatal("API reported success with no bucket")
	}
}

func TestIndexAndTraceFromS3(t *testing.T) {
	listing := `2026-11-10 02:15:03       4321 evals/days/2026-11-09/trace-s2.json
2026-11-10 02:15:02       4000 evals/days/2026-11-09/trace-s1.json
2026-11-10 02:15:02        300 evals/days/2026-11-09/open-code-s1.json
2026-11-10 02:15:01       4000 evals/days/2026-11-08/trace-s0.json
2026-11-10 02:15:01          9 evals/days/2026-11-08/readme.txt
`
	var calls []call
	s := &Store{Dir: t.TempDir(), S3: "s3://bkt/evals"}
	s.Run = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, args})
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ls --recursive"):
			return []byte(listing), nil
		case strings.HasSuffix(joined, "trace-s1.json -"):
			return []byte(`{"id":"s1"}`), nil
		}
		return nil, errors.New("unexpected " + joined)
	}
	refs, err := s.Index()
	must(t, err)
	want := []Ref{{"2026-11-08", "s0", false}, {"2026-11-09", "s1", true}, {"2026-11-09", "s2", false}}
	if len(refs) != 3 || refs[0] != want[0] || refs[1] != want[1] || refs[2] != want[2] {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	for i := 0; i < 3; i++ {
		b, err := s.Trace("2026-11-09", "s1")
		if err != nil || string(b) != `{"id":"s1"}` {
			t.Fatalf("trace: %q %v", b, err)
		}
	}
	fetches := 0
	for _, c := range calls {
		if strings.HasSuffix(strings.Join(c.args, " "), "trace-s1.json -") {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("trace fetched %d times; moving back and forth should be cached", fetches)
	}
}

func TestAnnotationFallsBackToTheBucketAndTreatsAbsentAsNone(t *testing.T) {
	s := &Store{Dir: t.TempDir(), S3: "s3://bkt/evals"}
	present := true
	s.Run = func(name string, args ...string) ([]byte, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(j, "s3 ls "):
			if present {
				return []byte("2026-11-10 02:00:00 10 evals/days/2026-11-09/open-code-s1.json\n"), nil
			}
			return nil, errors.New("exit 1") // aws s3 ls exits 1 when nothing matches
		case strings.Contains(j, "cp") && strings.HasSuffix(j, " -"):
			return []byte(`{"note":"from another machine"}`), nil
		}
		return nil, errors.New("unexpected " + j)
	}
	b, err := s.Annotation("2026-11-09", "s1")
	if err != nil || !strings.Contains(string(b), "another machine") {
		t.Fatalf("got %q, %v", b, err)
	}
	present = false
	if b, err := s.Annotation("2026-11-09", "s1"); err != nil || b != nil {
		t.Fatalf("absent annotation: %q %v; want nil, nil", b, err)
	}
}

func TestPageIsServedAndEmbedded(t *testing.T) {
	s, _ := newStore(t)
	h := NewHandler(s, []string{host})
	w := do(h, "GET", "/", "", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}
