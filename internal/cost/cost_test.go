package cost

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }

var prices = Prices{
	"haiku":  {Input: 0.10, Output: 0.50, LongPromptTokens: 100000, LongInput: 0.50, LongOutput: 2.50},
	"sonnet": {Input: 2, Output: 10, CacheRead: f(0.20)},
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostMath(t *testing.T) {
	r := prices.Cost("sonnet", Usage{Input: 1_000_000, Output: 100_000, APICalls: 10})
	if !r.Complete || !near(r.USD, 2+1) {
		t.Fatalf("sonnet: %+v", r)
	}
	r = prices.Cost("haiku", Usage{Input: 1_000_000, Output: 1_000_000, APICalls: 50}) // 20k per call: base tier
	if !near(r.USD, 0.10+0.50) || len(r.Notes) != 0 {
		t.Fatalf("haiku base: %+v", r)
	}
}

func TestLongPromptTierIsAppliedAndSaid(t *testing.T) {
	r := prices.Cost("haiku", Usage{Input: 1_000_000, Output: 1_000_000, APICalls: 5}) // 200k per call
	if !near(r.USD, 0.50+2.50) {
		t.Fatalf("long tier not applied: %+v", r)
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "estimate") {
		t.Fatalf("the estimate must be admitted: %+v", r.Notes)
	}
}

func TestUnknownModelIsNeverFree(t *testing.T) {
	r := prices.Cost("mystery", Usage{Input: 5_000_000, Output: 1})
	if r.Complete || len(r.Notes) == 0 {
		t.Fatalf("an unpriced model must be flagged, got %+v", r)
	}
}

func TestUnpublishedCachePriceIsPartialNotZero(t *testing.T) {
	r := prices.Cost("sonnet", Usage{Input: 100, CacheRead: 1_000_000, CacheWrite: 500})
	if r.Complete {
		t.Fatal("cache write has no price: result must be partial")
	}
	if !near(r.USD, 100*2/1e6+0.20) {
		t.Fatalf("priced parts must still count: %v", r.USD)
	}
}

func TestLoadPricesRejectsBadFiles(t *testing.T) {
	d := t.TempDir()
	for name, body := range map[string]string{
		"empty":   "models: {}\n",
		"zero":    "models:\n  m: {input: 0, output: 1}\n",
		"typo":    "models:\n  m: {input: 1, output: 1, outptu: 2}\n",
		"missing": "",
	} {
		p := filepath.Join(d, name)
		if body != "" {
			os.WriteFile(p, []byte(body), 0o600)
		}
		if _, err := LoadPrices(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadPrices("../../config/pricing.yaml"); err != nil {
		t.Fatalf("the shipped price table must load: %v", err)
	}
}

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func sess(id string, quiet time.Duration, ended bool, in int64) Session {
	return Session{ID: id, Source: "telegram", Model: "sonnet", Started: now.Add(-quiet - time.Hour), LastActive: now.Add(-quiet),
		Ended: ended, Usage: Usage{Input: in, Output: 10, APICalls: 2}}
}

func opts(st State) Options {
	return Options{Prices: prices, State: st, Now: now, Idle: 35 * time.Minute, Since: 24 * time.Hour, Zone: time.UTC}
}

func ids(ls []Line) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.S.ID)
	}
	return strings.Join(s, ",")
}

func TestDue(t *testing.T) {
	ss := []Session{
		sess("active", 5*time.Minute, false, 100), // still talking: not yet
		sess("idle", 40*time.Minute, false, 100),  // quiet past the limit: finished
		sess("ended", 5*time.Minute, true, 100),   // Hermes ended it: finished now
		sess("ancient", 30*time.Hour, true, 100),  // too old to announce
		sess("done", 3*time.Hour, true, 100),      // already reported
	}
	st := State{"done": {Tokens: 110}}
	if got := ids(Due(ss, opts(st))); got != "idle,ended" {
		t.Fatalf("due = %q", got)
	}
}

func TestResumedSessionIsReportedAsGrowth(t *testing.T) {
	s := sess("s", time.Hour, true, 1_000_000)
	first := prices.Cost("sonnet", s.Usage).USD
	st := State{"s": {Tokens: s.Usage.Input + s.Usage.Output, USD: first}}
	if len(Due([]Session{s}, opts(st))) != 0 {
		t.Fatal("unchanged session must not be sent twice")
	}
	s.Usage.Input += 500_000
	d := Due([]Session{s}, opts(st))
	if len(d) != 1 || !d[0].Continue || !near(d[0].Delta, 1.0) {
		t.Fatalf("growth: %+v", d)
	}
}

type fake struct {
	sent []string
	fail error
}

func (f *fake) Send(_ context.Context, text string) error {
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, text)
	return nil
}

func TestFailedSendIsNotRecordedAndIsRetried(t *testing.T) {
	sp := filepath.Join(t.TempDir(), "cost-reported.json")
	ss := []Session{sess("a", time.Hour, true, 100)}
	st := State{}
	fk := &fake{fail: errors.New("telegram down")}
	if _, err := Run(context.Background(), ss, opts(st), fk, sp); err == nil {
		t.Fatal("a failed send must surface")
	}
	if len(st) != 0 {
		t.Fatal("a session that was not delivered must not be marked reported")
	}
	fk.fail = nil
	st2, _ := LoadState(sp)
	sent, err := Run(context.Background(), ss, opts(st2), fk, sp)
	if err != nil || len(sent) != 1 {
		t.Fatalf("retry: %v %v", sent, err)
	}
	st3, _ := LoadState(sp)
	if again, _ := Run(context.Background(), ss, opts(st3), fk, sp); len(again) != 0 {
		t.Fatal("sent twice")
	}
	if fi, _ := os.Stat(sp); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v", fi.Mode())
	}
}

func TestMessageEscapesAndStatesWhatIsMissing(t *testing.T) {
	s := sess("<b>x</b>&", time.Hour, true, 100)
	s.Model = "mystery"
	l := Line{S: s, R: prices.Cost("mystery", s.Usage)}
	m := Message(l, 1.5, 3, true)
	if strings.Contains(m, "<b>x") || !strings.Contains(m, "&lt;b&gt;x") {
		t.Fatalf("ids must be HTML-escaped for Telegram: %s", m)
	}
	for _, want := range []string{"(partial)", "no price for model", "Today: $1.50 over 3 sessions", "not priced"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in:\n%s", want, m)
		}
	}
}

func TestParseSessionsHandlesNulls(t *testing.T) {
	ss, err := ParseSessions([]byte(`[{"id":"a","source":null,"model":null,"started_at":1000.5,"ended_at":null,"last_activity_at":null,"last_msg":2000,
	"message_count":null,"tool_call_count":null,"input_tokens":5,"output_tokens":null,"cache_read_tokens":null,"cache_write_tokens":null,"api_call_count":null}]`))
	if err != nil || len(ss) != 1 {
		t.Fatal(ss, err)
	}
	if ss[0].Ended || ss[0].Usage.Input != 5 || ss[0].LastActive.Unix() != 2000 {
		t.Fatalf("%+v", ss[0])
	}
	if ss, err := ParseSessions(nil); err != nil || ss != nil {
		t.Fatal("empty output is no sessions, not an error")
	}
}

func TestTodayUsesTheZoneDay(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	o := opts(State{})
	o.Zone = ist
	o.Now = time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC) // 01:30 on the 16th in IST
	a := sess("a", 0, true, 1_000_000)
	a.Started = time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC) // 00:30 on the 16th IST: today
	b := sess("b", 0, true, 3_000_000)
	b.Started = time.Date(2026, 11, 15, 17, 0, 0, 0, time.UTC) // 22:30 on the 15th IST: yesterday
	usd, n, _ := Today([]Session{a, b}, o)
	if n != 1 || !near(usd, 2.0001) {
		t.Fatalf("n=%d usd=%v", n, usd)
	}
}
