package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/health"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
)

var (
	ist     = func() *time.Location { l, _ := time.LoadLocation("Asia/Kolkata"); return l }()
	hNow    = time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)
	hMidnig = func(day int) time.Time { return time.Date(2026, 11, day, 0, 0, 0, 0, ist).UTC() }
)

type fakeReader struct {
	ms      []health.Metric
	acts    []health.Activity
	run     *health.Run
	weeks   []health.Week
	err     error
	gotFrom time.Time
	gotTo   time.Time
}

func (f *fakeReader) Range(_ context.Context, from, to time.Time) ([]health.Metric, error) {
	f.gotFrom, f.gotTo = from, to
	return f.ms, f.err
}
func (f *fakeReader) ActivitiesIn(context.Context, time.Time, time.Time) ([]health.Activity, error) {
	return f.acts, f.err
}
func (f *fakeReader) LastRun(context.Context) (*health.Run, error) { return f.run, f.err }
func (f *fakeReader) Weekly(_ context.Context, _ string, from, to time.Time) ([]health.Week, error) {
	f.gotFrom, f.gotTo = from, to
	return f.weeks, f.err
}

func healthDeps(t *testing.T, r *fakeReader) Deps {
	d := deps(t)
	d.Health, d.HealthLoc, d.Now = r, ist, func() time.Time { return hNow }
	return d
}

func TestWithoutAHealthStoreThereAreNoHealthTools(t *testing.T) {
	for _, r := range All(deps(t)) {
		if strings.HasPrefix(r.Name, "health.") {
			t.Fatalf("%s exists with no store configured", r.Name)
		}
	}
}

func TestHealthToolsAreReadOnlyTierZeroAndTrusted(t *testing.T) {
	regs := All(healthDeps(t, &fakeReader{}))
	var n int
	for _, r := range regs {
		if !strings.HasPrefix(r.Name, "health.") {
			continue
		}
		n++
		if r.Tier != policy.TierInternal || r.Kind != policy.Read || r.Trust != mcpsrv.Trusted || r.Description == "" {
			t.Errorf("%s: %d/%v/%q", r.Name, r.Tier, r.Kind, r.Trust)
		}
	}
	if n != 3 {
		t.Fatalf("%d health tools, want get_summary, get_series and get_digest", n)
	}
}

func TestSummaryAsksForTheRightWindowAndReturnsTheNumbers(t *testing.T) {
	r := &fakeReader{ms: []health.Metric{{Time: hMidnig(21), Name: "steps", Value: 9000}, {Time: hMidnig(22), Name: "steps", Value: 3000}},
		run: &health.Run{Status: "ok", FinishedAt: hNow.Add(-2 * time.Hour)}}
	out, err := find(t, All(healthDeps(t, r)), "health.get_summary").Handle(context.Background(), map[string]any{"days": float64(7)})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(health.Summary)
	if len(s.Metrics) != 1 || s.Metrics[0].Mean != 6000 || s.LastSync == nil || s.LastSync.HoursAgo != 2 || s.Stale {
		t.Fatalf("%+v", s)
	}
	if !r.gotFrom.Equal(hMidnig(16)) || !r.gotTo.After(hNow) {
		t.Fatalf("window %v to %v: seven local days ending today start at the 16th", r.gotFrom, r.gotTo)
	}
}

func TestSummaryDefaultsToSevenDaysAndRefusesNonsense(t *testing.T) {
	h := find(t, All(healthDeps(t, &fakeReader{})), "health.get_summary").Handle
	if _, err := h(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"zero": float64(0), "too many": float64(91), "fraction": 1.5, "a string": "7", "negative": float64(-3)} {
		if _, err := h(context.Background(), map[string]any{"days": v}); err == nil || !strings.Contains(err.Error(), "days") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSummaryOfNoDataSaysSoAndIsStale(t *testing.T) {
	out, err := find(t, All(healthDeps(t, &fakeReader{})), "health.get_summary").Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := out.(health.Summary); !s.Stale || s.Note == "" || len(s.Metrics) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestAStoreErrorIsATextualErrorWithoutLeakingNothingSecret(t *testing.T) {
	_, err := find(t, All(healthDeps(t, &fakeReader{err: errors.New("connection refused")})), "health.get_summary").Handle(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Fatal(err)
	}
}

func TestSeriesDayAndWeek(t *testing.T) {
	r := &fakeReader{ms: []health.Metric{{Time: hMidnig(20), Name: "hr_resting", Value: 54}, {Time: hMidnig(21), Name: "hr_resting", Value: 56}, {Time: hMidnig(21), Name: "steps", Value: 1}},
		weeks: []health.Week{{Start: hMidnig(16), Mean: 55, Min: 54, Max: 56, Days: 2}}}
	h := find(t, All(healthDeps(t, r)), "health.get_series").Handle
	out, err := h(context.Background(), map[string]any{"metric": "hr_resting", "days": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	days := out.(map[string]any)["days"].([]health.DayValue)
	if len(days) != 2 || days[0].Day != "2026-11-20" || days[1].Value != 56 {
		t.Fatalf("%+v", days)
	}
	out, err = h(context.Background(), map[string]any{"metric": "hr_resting", "bucket": "week"})
	if err != nil {
		t.Fatal(err)
	}
	if w := out.(map[string]any)["weeks"].([]health.Week); len(w) != 1 || w[0].Days != 2 {
		t.Fatalf("%+v", w)
	}
}

func TestSeriesRefusesAnUnknownMetricAndListsTheRealOnes(t *testing.T) {
	h := find(t, All(healthDeps(t, &fakeReader{})), "health.get_series").Handle
	_, err := h(context.Background(), map[string]any{"metric": "blood_pressure"})
	if err == nil || !strings.Contains(err.Error(), "steps") || !strings.Contains(err.Error(), "hr_resting") {
		t.Fatalf("%v", err)
	}
	for name, a := range map[string]map[string]any{
		"no metric":  {},
		"bad bucket": {"metric": "steps", "bucket": "month"},
		"too long":   {"metric": "steps", "days": float64(366)},
	} {
		if _, err := h(context.Background(), a); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
