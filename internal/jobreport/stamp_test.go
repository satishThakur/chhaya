package jobreport

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 22, 21, 10, 0, 0, time.UTC)

func TestStampRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "job.json")
	if err := WriteStamp(p, Stamp{Time: t0, Detail: "read 2"}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadStamp(p)
	if err != nil || !s.Time.Equal(t0) || s.Detail != "read 2" || s.Failed != "" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestMissingOrEmptyStampIsAnErrorNotAPass(t *testing.T) {
	d := t.TempDir()
	if _, err := ReadStamp(filepath.Join(d, "none.json")); err == nil {
		t.Fatal("a job that never ran must not read as fine")
	}
	for name, body := range map[string]string{"bad.json": "{", "empty.json": "{}"} {
		p := filepath.Join(d, name)
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := ReadStamp(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTheBackupsOwnFileReadsAsAStamp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "last-success.json")
	os.WriteFile(p, []byte(`{"time":"2026-11-22T15:00:00Z","archive":"chhaya-data-x.tar.gz"}`), 0o600)
	s, err := ReadStamp(p)
	if err != nil || s.Detail != "chhaya-data-x.tar.gz" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestAFailureSurvivesALaterGoodRunInTheSameBatchOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.json")
	WriteStamp(p, Stamp{Time: t0, Failed: "gateway unreachable"})
	WriteStamp(p, Stamp{Time: t0.Add(2 * time.Minute), Detail: "ok"})
	if s, _ := ReadStamp(p); s.Failed != "gateway unreachable" {
		t.Fatalf("the second run erased the first run's failure: %+v", s)
	}
	WriteStamp(p, Stamp{Time: t0.Add(24 * time.Hour), Detail: "ok"})
	if s, _ := ReadStamp(p); s.Failed != "" {
		t.Fatalf("a failure from the day before must not stick: %+v", s)
	}
}
