package jobreport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSend struct {
	got []string
	err error
}

func (f *fakeSend) Send(_ context.Context, s string) error { f.got = append(f.got, s); return f.err }

func runner(log *[]string, fail string) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		*log = append(*log, name+" "+strings.Join(args, " "))
		if fail != "" && len(args) > 1 && args[1] == fail {
			return nil, errors.New("boom")
		}
		if len(args) > 1 && args[1] == "presign" {
			return []byte("https://signed.example/r?sig=1\n"), nil
		}
		return nil, nil
	}
}

func msg(link string) string { return "Job done: 3 things\nreport: " + link + "\n" }

func TestDeliverUploadsPresignsAndSendsOneSummaryWithTheLink(t *testing.T) {
	var log []string
	s := &fakeSend{}
	m, err := Deliver(context.Background(), Options{S3: "s3://b/evals", Expires: 6 * time.Hour, Run: runner(&log, ""), Send: s, Dir: t.TempDir()},
		"dream-2026-11-19", "<html>x</html>", msg)
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(log, "\n")
	for _, want := range []string{"s3 cp", "s3://b/evals/reports/dream-2026-11-19.html", "text/html", "--expires-in 21600"} {
		if !strings.Contains(j, want) {
			t.Errorf("%q missing from:\n%s", want, j)
		}
	}
	if len(s.got) != 1 || s.got[0] != m || !strings.Contains(m, "https://signed.example/r?sig=1") || !strings.Contains(m, "link works for 6h0m0s") {
		t.Fatalf("sent %v, msg %q", s.got, m)
	}
}

func TestAFailedUploadStillSendsTheSummaryAndSaysSo(t *testing.T) {
	var log []string
	s := &fakeSend{}
	m, err := Deliver(context.Background(), Options{S3: "s3://b/evals", Expires: time.Hour, Run: runner(&log, "cp"), Send: s, Dir: t.TempDir()}, "x", "<html/>", msg)
	if err == nil || len(s.got) != 1 || !strings.Contains(m, "report not uploaded") {
		t.Fatalf("err %v sent %v msg %q", err, s.got, m)
	}
}

func TestAFailedSendIsAnError(t *testing.T) {
	var log []string
	if _, err := Deliver(context.Background(), Options{S3: "s3://b/evals", Expires: time.Hour, Run: runner(&log, ""), Send: &fakeSend{err: errors.New("tg down")}, Dir: t.TempDir()}, "x", "<html/>", msg); err == nil {
		t.Fatal("a send failure must not be silent")
	}
}

func TestWithoutABucketOrASenderNothingLeavesTheMachine(t *testing.T) {
	var log []string
	m, err := Deliver(context.Background(), Options{Run: runner(&log, ""), Dir: t.TempDir()}, "x", "<html/>", msg)
	if err != nil || m == "" || len(log) != 0 {
		t.Fatalf("msg %q err %v log %v", m, err, log)
	}
}

func TestANameThatCouldEscapeTheReportsFolderIsRefused(t *testing.T) {
	var log []string
	for _, name := range []string{"../x", "a/b", "", "x y"} {
		if _, err := Deliver(context.Background(), Options{S3: "s3://b/evals", Expires: time.Hour, Run: runner(&log, ""), Dir: t.TempDir()}, name, "<html/>", msg); err == nil {
			t.Errorf("name %q must be refused", name)
		}
	}
	if len(log) != 0 {
		t.Fatalf("nothing may be uploaded under a bad name: %v", log)
	}
}
