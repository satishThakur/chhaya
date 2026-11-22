package health

import (
	"testing"
	"time"
)

func day(n int) time.Time { // IST midnight of 2026-11-<n>, as stored
	return time.Date(2026, 11, n, 0, 0, 0, 0, ist).UTC()
}

func pts(metric string, vals ...float64) []Metric {
	var out []Metric
	for i, v := range vals {
		out = append(out, Metric{Time: day(16 + i), Name: metric, Value: v})
	}
	return out
}

func find(t *testing.T, s Summary, metric string) MetricSummary {
	t.Helper()
	for _, m := range s.Metrics {
		if m.Metric == metric {
			return m
		}
	}
	t.Fatalf("no %s in %+v", metric, s.Metrics)
	return MetricSummary{}
}

func TestSummaryReportsTheQuantitiesNotAVerdict(t *testing.T) {
	// 7 days, 16th to 22nd; "now" is the 22nd IST.
	ms := pts("sleep_total_seconds", 25200, 27000, 21600, 28800, 25200, 30600, 23400)
	s := Summarize(ms, nil, nil, 7, ist, now)
	m := find(t, s, "sleep_total_seconds")
	if m.Days != 7 || m.Min != 21600 || m.Max != 30600 || m.Latest.Day != "2026-11-22" || m.Latest.Value != 23400 {
		t.Fatalf("%+v", m)
	}
	if want := (25200.0 + 27000 + 21600 + 28800 + 25200 + 30600 + 23400) / 7; m.Mean != want {
		t.Fatalf("mean %v want %v", m.Mean, want)
	}
	if m.Unit != "seconds" {
		t.Fatalf("unit %q", m.Unit)
	}
	if s.Days != 7 || s.WithData != 7 || s.Stale {
		t.Fatalf("%+v", s)
	}
}

func TestTrendNeedsSixDaysAndShowsBothHalves(t *testing.T) {
	s := Summarize(pts("steps", 4000, 4000, 4000, 8000, 8000, 8000), nil, nil, 7, ist, now)
	m := find(t, s, "steps")
	if m.EarlierMean == nil || m.LaterMean == nil || *m.EarlierMean != 4000 || *m.LaterMean != 8000 {
		t.Fatalf("%+v", m)
	}
	s = Summarize(pts("steps", 4000, 8000, 8000), nil, nil, 7, ist, now)
	if m := find(t, s, "steps"); m.EarlierMean != nil || m.LaterMean != nil {
		t.Fatalf("three points do not make a trend: %+v", m)
	}
}

func TestOldDataIsStaleAndSaysHowOld(t *testing.T) {
	ms := []Metric{{Time: day(17), Name: "steps", Value: 1000}, {Time: day(18), Name: "steps", Value: 1000}}
	s := Summarize(ms, nil, nil, 7, ist, now)
	if !s.Stale || s.LatestDay != "2026-11-18" || s.DaysBehind != 4 {
		t.Fatalf("%+v", s)
	}
}

func TestNoDataSaysSoPlainly(t *testing.T) {
	s := Summarize(nil, nil, nil, 7, ist, now)
	if len(s.Metrics) != 0 || s.WithData != 0 || !s.Stale || s.Note == "" {
		t.Fatalf("%+v", s)
	}
}

func TestActivitiesAreCountedByTypeAndTotalled(t *testing.T) {
	a := []Activity{
		{Time: day(20).Add(2 * time.Hour), Type: "running", DurationSeconds: 3000},
		{Time: day(21).Add(2 * time.Hour), Type: "running", DurationSeconds: 1800},
		{Time: day(21).Add(5 * time.Hour), Type: "strength_training", DurationSeconds: 2400},
	}
	s := Summarize(pts("steps", 1, 2, 3), a, nil, 7, ist, now)
	if s.Activities.Count != 3 || s.Activities.TotalMinutes != 120 || s.Activities.ByType["running"] != 2 || s.Activities.ByType["strength_training"] != 1 {
		t.Fatalf("%+v", s.Activities)
	}
}

func TestLastSyncIsReported(t *testing.T) {
	run := &Run{FinishedAt: now.Add(-3 * time.Hour), Status: "partial", Errors: []string{"stress: 500"}}
	s := Summarize(pts("steps", 1), nil, run, 7, ist, now)
	if s.LastSync == nil || s.LastSync.Status != "partial" || s.LastSync.HoursAgo != 3 || len(s.LastSync.Errors) != 1 {
		t.Fatalf("%+v", s.LastSync)
	}
}

func TestMetricsComeOutInAFixedOrder(t *testing.T) {
	ms := append(pts("steps", 1, 2), pts("hr_resting", 50, 51)...)
	ms = append(ms, pts("sleep_total_seconds", 1, 2)...)
	a := Summarize(ms, nil, nil, 7, ist, now)
	b := Summarize(ms, nil, nil, 7, ist, now)
	if len(a.Metrics) != 3 || a.Metrics[0].Metric != b.Metrics[0].Metric || a.Metrics[0].Metric > a.Metrics[1].Metric {
		t.Fatalf("%+v", a.Metrics)
	}
}

func TestSeriesListsDaysOldestFirstAndIgnoresOtherMetrics(t *testing.T) {
	ms := append(pts("steps", 3, 1, 2), pts("hr_resting", 50)...)
	got := SeriesOf(ms, "steps", ist)
	if len(got) != 3 || got[0].Day != "2026-11-16" || got[2].Day != "2026-11-18" || got[1].Value != 1 {
		t.Fatalf("%+v", got)
	}
}
