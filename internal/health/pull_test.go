package health

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 answers the three aws calls the puller makes.
type fakeS3 struct {
	mu     sync.Mutex
	inbox  map[string]string
	moved  map[string]string // name -> processed|rejected
	failLS bool
	gone   map[string]bool // objects another pull takes between the listing and the read
}

func (f *fakeS3) run(name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "aws" || len(args) < 2 || (args[0] != "s3" && args[0] != "s3api") {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	last := args[len(args)-1]
	switch {
	case args[0] == "s3api":
		if f.failLS {
			return nil, fmt.Errorf("AccessDenied")
		}
		if len(f.inbox) == 0 {
			return []byte("None\n"), nil
		}
		var keys []string
		for n := range f.inbox {
			keys = append(keys, "health/inbox/"+n)
		}
		return []byte(strings.Join(keys, "\t") + "\n"), nil
	case args[1] == "cp":
		if g := args[len(args)-2]; f.gone[g[strings.LastIndex(g, "/")+1:]] {
			return nil, fmt.Errorf("aws: exit status 1: download failed: An error occurred (404) when calling the HeadObject operation: Not Found")
		}
		n := args[len(args)-2][strings.LastIndex(args[len(args)-2], "/")+1:]
		return []byte(f.inbox[n]), nil
	case args[1] == "mv":
		src, dst := args[len(args)-2], last
		n := src[strings.LastIndex(src, "/")+1:]
		to := dst[strings.Index(dst, "health/")+len("health/"):]
		f.moved[n] = strings.SplitN(to, "/", 2)[0]
		delete(f.inbox, n)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected aws s3 %s", args[1])
}

func newFake(files map[string]string) *fakeS3 { return &fakeS3{inbox: files, moved: map[string]string{}} }

func gateway(t *testing.T, code func(path, body string) (int, string)) (*httptest.Server, *[]string) {
	var seen []string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ingest-Secret") != "the-secret-the-secret" {
			w.WriteHeader(401)
			return
		}
		b := make([]byte, 1000)
		n, _ := r.Body.Read(b)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+string(b[:n]))
		mu.Unlock()
		c, msg := code(r.URL.Path, string(b[:n]))
		w.WriteHeader(c)
		fmt.Fprint(w, msg)
	}))
	t.Cleanup(ts.Close)
	return ts, &seen
}

func puller(f *fakeS3, url string) *Puller {
	return &Puller{S3: "s3://b/health", URL: url, Secret: "the-secret-the-secret", Run: f.run}
}

func TestPullIngestsDaysBeforeRunsAndFilesThem(t *testing.T) {
	f := newFake(map[string]string{"run-20261122T060000Z-aa.json": `{"r":1}`, "day-2026-11-22-bb.json": `{"d":2}`, "day-2026-11-21-cc.json": `{"d":1}`})
	ts, seen := gateway(t, func(string, string) (int, string) { return 200, "{}" })
	r, err := puller(f, ts.URL).Pull(context.Background())
	if err != nil || r.Ingested != 3 || len(r.Rejected) != 0 {
		t.Fatalf("got %+v, %v", r, err)
	}
	want := []string{`/ingest/day {"d":1}`, `/ingest/day {"d":2}`, `/ingest/run {"r":1}`}
	for i := range want {
		if (*seen)[i] != want[i] {
			t.Fatalf("order: %v", *seen)
		}
	}
	if len(f.inbox) != 0 || len(f.moved) != 3 {
		t.Fatalf("inbox %v moved %v", f.inbox, f.moved)
	}
	for _, to := range f.moved {
		if to != "processed" {
			t.Fatalf("filed under %q", to)
		}
	}
}

func TestPullRejectedDocumentIsFiledAndTheRestStillGoes(t *testing.T) {
	f := newFake(map[string]string{"day-2026-11-21-aa.json": `bad`, "day-2026-11-22-bb.json": `good`})
	ts, _ := gateway(t, func(_, b string) (int, string) {
		if b == "bad" {
			return 400, `unknown field "average_hrv"`
		}
		return 200, "{}"
	})
	r, err := puller(f, ts.URL).Pull(context.Background())
	if err != nil || r.Ingested != 1 || len(r.Rejected) != 1 || !strings.Contains(r.Rejected[0], "average_hrv") {
		t.Fatalf("got %+v, %v", r, err)
	}
	if f.moved["day-2026-11-21-aa.json"] != "rejected" || f.moved["day-2026-11-22-bb.json"] != "processed" {
		t.Fatalf("moved %v", f.moved)
	}
}

func TestPullLeavesObjectsWhenTheGatewayCannotTakeThem(t *testing.T) {
	for name, code := range map[string]int{"down": 503, "wrong secret": 401, "broken": 500} {
		t.Run(name, func(t *testing.T) {
			f := newFake(map[string]string{"day-2026-11-22-aa.json": `x`, "day-2026-11-23-bb.json": `y`})
			ts, seen := gateway(t, func(string, string) (int, string) { return code, "no" })
			r, err := puller(f, ts.URL).Pull(context.Background())
			if err == nil || len(f.inbox) != 2 || len(f.moved) != 0 || r.Left != 2 {
				t.Fatalf("got %+v, %v; inbox %v", r, err, f.inbox)
			}
			if len(*seen) > 1 {
				t.Fatalf("kept going after a refusal that is not about the document: %v", *seen)
			}
		})
	}
}

func TestPullUnreachableGatewayLeavesEverything(t *testing.T) {
	f := newFake(map[string]string{"day-2026-11-22-aa.json": `x`})
	ts, _ := gateway(t, nil)
	url := ts.URL
	ts.Close()
	if _, err := puller(f, url).Pull(context.Background()); err == nil || len(f.inbox) != 1 {
		t.Fatalf("err %v inbox %v", err, f.inbox)
	}
}

func TestPullRefusesNamesTheSyncDoesNotWrite(t *testing.T) {
	f := newFake(map[string]string{"../../etc.json": "x", "notes.txt": "x", "day-2026-11-22-aa.json": "ok"})
	ts, seen := gateway(t, func(string, string) (int, string) { return 200, "{}" })
	r, err := puller(f, ts.URL).Pull(context.Background())
	if err != nil || r.Ingested != 1 || len(r.Rejected) != 2 || len(*seen) != 1 {
		t.Fatalf("got %+v, %v, %v", r, err, *seen)
	}
}

func TestPullCapsAPassAndReportsWhatIsLeft(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("day-2026-11-%02d-aa.json", 10+i)] = "x"
	}
	f := newFake(files)
	ts, _ := gateway(t, func(string, string) (int, string) { return 200, "{}" })
	p := puller(f, ts.URL)
	p.Max = 2
	r, err := p.Pull(context.Background())
	if err != nil || r.Ingested != 2 || r.Left != 3 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestPullEmptyInboxIsNotAnError(t *testing.T) {
	r, err := puller(newFake(nil), "http://127.0.0.1:1").Pull(context.Background())
	if err != nil || r.Ingested != 0 || r.Left != 0 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestPullListFailureIsAnError(t *testing.T) {
	f := newFake(nil)
	f.failLS = true
	if _, err := puller(f, "http://127.0.0.1:1").Pull(context.Background()); err == nil {
		t.Fatal("a listing that failed must not read as an empty inbox")
	}
}

func TestPullSkipsAnObjectAnotherPullTookFirst(t *testing.T) {
	f := newFake(map[string]string{"day-2026-11-21-aa.json": "x", "day-2026-11-22-bb.json": "y"})
	f.gone = map[string]bool{"day-2026-11-21-aa.json": true}
	ts, seen := gateway(t, func(string, string) (int, string) { return 200, "{}" })
	r, err := puller(f, ts.URL).Pull(context.Background())
	if err != nil || r.Ingested != 1 || len(*seen) != 1 {
		t.Fatalf("a vanished object must be skipped, not fail the pass: %+v %v", r, err)
	}
}
