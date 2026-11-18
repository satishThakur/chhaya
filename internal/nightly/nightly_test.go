package nightly

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/memtest"
)

var now = time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)

func yes() *bool { b := true; return &b }

func goodRun() *RunFile {
	return &RunFile{RanAt: now.Add(-5 * time.Hour), Model: "claude-haiku-5-5", Result: memrun.Result{
		Probes: []memrun.ProbeResult{{ID: "p1", Fact: "f1", Retrieved: true, Judged: yes(), In: 100, Out: 10}},
		Golden: []memrun.GoldenResult{{ID: "g1", Pending: true}},
	}}
}

func goodInput() Input {
	return Input{
		Days:    []string{"2026-11-17", "2026-11-18"},
		Results: []check.TraceResult{{ID: "t1", Day: "2026-11-17", Source: "telegram"}},
		Memtest: &memtest.Report{Facts: []memtest.Fact{{ID: "a"}}, Probes: []memtest.Probe{{ID: "p1"}}},
		Run:     goodRun(),
		Now:     now,
	}
}

func TestCleanNightSaysOk(t *testing.T) {
	in := goodInput()
	if got := in.Verdict(); got != OK {
		t.Fatalf("verdict %v, want ok", got)
	}
	m := Message(in, "")
	if !strings.Contains(m, "all clear") || !strings.Contains(m, "2026-11-18") {
		t.Fatalf("message: %q", m)
	}
}

func TestFlagsAreCountedByName(t *testing.T) {
	in := goodInput()
	in.Results[0].Findings = []check.Finding{
		{Check: "recommend.unsearched", Level: check.Flag, MessageID: 4, Detail: "SECRET-DETAIL asked for a podcast"},
		{Check: "recommend.unsearched", Level: check.Flag, Detail: "SECRET-DETAIL two"},
		{Check: "reply.owner_correction", Level: check.Flag, Detail: "SECRET-DETAIL three"},
	}
	if in.Verdict() != Flagged {
		t.Fatalf("want flagged, got %v", in.Verdict())
	}
	m := Message(in, "https://x")
	for _, want := range []string{"recommend.unsearched ×2", "reply.owner_correction ×1", "https://x"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "SECRET-DETAIL") {
		t.Fatalf("a finding's text reached the Telegram message:\n%s", m)
	}
}

func TestFailuresAndNotRunAreNotOk(t *testing.T) {
	cases := map[string]func(*Input){
		"check fail": func(in *Input) {
			in.Results[0].Findings = []check.Finding{{Check: "x", Level: check.Fail, Detail: "d"}}
		},
		"unreadable":      func(in *Input) { in.Problems = []string{"a/b: bad"} },
		"memtest fail":    func(in *Input) { in.Memtest.Problems = []memtest.Problem{{Level: "fail", Msg: "dup id"}} },
		"memtest missing": func(in *Input) { in.Memtest = nil },
		"canary missing":  func(in *Input) { in.Run = nil },
		"canary stale":    func(in *Input) { in.Run.RanAt = now.Add(-30 * time.Hour) },
		"MISS": func(in *Input) {
			in.Run.Result.Probes[0].Retrieved = false
		},
		"golden fail": func(in *Input) { in.Run.Result.Golden[0].Pending = false },
	}
	for name, f := range cases {
		in := goodInput()
		f(&in)
		if in.Verdict() != Failed {
			t.Errorf("%s: verdict %v, want failed", name, in.Verdict())
		}
		if m := Message(in, ""); strings.Contains(m, "all clear") {
			t.Errorf("%s: said all clear: %s", name, m)
		}
	}
}

func TestJudgeWrongDoesNotChangeVerdict(t *testing.T) {
	in := goodInput()
	no := false
	in.Run.Result.Probes[0].Judged = &no
	if in.Verdict() != OK {
		t.Fatalf("a judge verdict alone changed the verdict: %v", in.Verdict())
	}
	if !strings.Contains(Message(in, ""), "judge 1") {
		t.Fatalf("judge count not shown: %s", Message(in, ""))
	}
}

func TestHTMLEscapesAndShowsDetail(t *testing.T) {
	in := goodInput()
	in.Checks = check.All()
	name := in.Checks[0].Name
	in.Results[0].Ran = []string{name}
	in.Results[0].Findings = []check.Finding{{Check: name, Level: check.Flag, MessageID: 3, Detail: `<script>alert(1)</script> podcast`}}
	h := HTML(in)
	if strings.Contains(h, "<script>alert") {
		t.Fatal("finding text was not escaped")
	}
	if !strings.Contains(h, "&lt;script&gt;") || !strings.Contains(h, "t1") {
		t.Fatalf("detail or trace id missing:\n%s", h)
	}
}

func TestHTMLHasHeaderSectionsAndARowPerTest(t *testing.T) {
	in := goodInput()
	in.Checks = check.All()
	h := HTML(in)
	if !strings.Contains(h, "Overall: PASS") {
		t.Fatalf("no overall header:\n%s", h)
	}
	for _, title := range []string{"Trace checks", "Memory tests: structure", "Memory canary: retrieval", "Memory canary: golden questions", "Memory canary: judge"} {
		if !strings.Contains(h, title) {
			t.Errorf("section %q missing", title)
		}
	}
	// Every section is collapsed: none carries the open attribute.
	if strings.Contains(h, "<details class=\"sec\" open") || strings.Contains(h, "<details class=\"row\" open") {
		t.Error("sections and rows must start collapsed")
	}
	// Every check is listed even when it found nothing.
	for _, c := range check.All() {
		if !strings.Contains(h, c.Name) {
			t.Errorf("check %s not listed", c.Name)
		}
	}
	if !strings.Contains(h, "Deterministic") || !strings.Contains(h, "Probabilistic") {
		t.Error("each row must say whether it is deterministic or probabilistic")
	}
}

func TestHTMLFailAndJudgeRows(t *testing.T) {
	in := goodInput()
	in.Run.Result.Probes[0].Retrieved = false
	no := false
	in.Run.Result.Probes = append(in.Run.Result.Probes, memrun.ProbeResult{ID: "p2", Fact: "f2", Retrieved: true, Judged: &no, JudgeWhy: "wrong book"})
	h := HTML(in)
	if !strings.Contains(h, "Overall: FAIL") {
		t.Fatal("a MISS must make the overall result FAIL")
	}
	secs := Sections(in)
	var judge Section
	for _, s := range secs {
		if strings.HasSuffix(s.Title, "judge") {
			judge = s
		}
	}
	if judge.Count(Fail) != 1 {
		t.Fatalf("the judge's verdict should show as a Fail row: %+v", judge.Rows)
	}
	for _, r := range judge.Rows {
		if r.Type != Probabilistic {
			t.Errorf("judge row %s is %s, want Probabilistic", r.Name, r.Type)
		}
	}
	in2 := goodInput()
	in2.Run.Result.Probes[0].Judged = &no
	if !strings.Contains(HTML(in2), "Overall: PASS") {
		t.Fatal("a judge Fail alone must not change the overall result")
	}
}

func TestHTMLNotRunIsNotAPass(t *testing.T) {
	in := goodInput()
	in.Run = nil
	h := HTML(in)
	if !strings.Contains(h, "Overall: FAIL") || !strings.Contains(h, "Not run") {
		t.Fatalf("a canary that did not run must show Not run and FAIL:\n%s", h)
	}
}

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

func TestPublishUploadsPresignsAndSends(t *testing.T) {
	var log []string
	s := &fakeSend{}
	msg, err := Publish(context.Background(), goodInput(), PublishOptions{S3: "s3://b/evals", Expires: 6 * time.Hour, Run: runner(&log, ""), Send: s, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(log, "\n")
	if !strings.Contains(j, "s3 cp") || !strings.Contains(j, "s3://b/evals/reports/2026-11-18.html") || !strings.Contains(j, "text/html") {
		t.Fatalf("upload: %s", j)
	}
	if !strings.Contains(j, "--expires-in 21600") {
		t.Fatalf("presign: %s", j)
	}
	if len(s.got) != 1 || s.got[0] != msg || !strings.Contains(msg, "https://signed.example/r?sig=1") {
		t.Fatalf("sent %v, msg %q", s.got, msg)
	}
}

func TestFailedUploadStillSendsTheMessage(t *testing.T) {
	var log []string
	s := &fakeSend{}
	msg, err := Publish(context.Background(), goodInput(), PublishOptions{S3: "s3://b/evals", Expires: time.Hour, Run: runner(&log, "cp"), Send: s, Dir: t.TempDir()})
	if err == nil {
		t.Fatal("an upload failure must be returned")
	}
	if len(s.got) != 1 || !strings.Contains(msg, "report not uploaded") {
		t.Fatalf("message must still go out, saying so: %q", msg)
	}
}

func TestFailedSendIsAnError(t *testing.T) {
	var log []string
	_, err := Publish(context.Background(), goodInput(), PublishOptions{S3: "s3://b/evals", Expires: time.Hour, Run: runner(&log, ""), Send: &fakeSend{err: errors.New("tg down")}, Dir: t.TempDir()})
	if err == nil {
		t.Fatal("a send failure must not be silent")
	}
}

func TestNoSendWhenSenderNil(t *testing.T) {
	var log []string
	msg, err := Publish(context.Background(), goodInput(), PublishOptions{Run: runner(&log, ""), Dir: t.TempDir()})
	if err != nil || msg == "" || len(log) != 0 {
		t.Fatalf("msg %q err %v log %v", msg, err, log)
	}
}

func TestRunFileRoundTripAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memrun.json")
	if _, err := ReadRunFile(p); err == nil {
		t.Fatal("a missing file must be an error")
	}
	if err := WriteRunFile(p, *goodRun()); err != nil {
		t.Fatal(err)
	}
	r, err := ReadRunFile(p)
	if err != nil || r.Model != "claude-haiku-5-5" || len(r.Result.Probes) != 1 || !r.Result.Probes[0].Retrieved {
		t.Fatalf("%+v %v", r, err)
	}
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := ReadRunFile(p); err == nil {
		t.Fatal("garbage must be an error")
	}
}
