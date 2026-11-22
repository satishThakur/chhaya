package health

import (
	"math"
	"strings"
	"testing"
	"time"
)

var (
	dIST = func() *time.Location { l, _ := time.LoadLocation("Asia/Kolkata"); return l }()
	// 07:15 IST on 23 Nov 2026.
	dNow = time.Date(2026, 11, 23, 1, 45, 0, 0, time.UTC)
)

func dgDay(d string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", d, dIST)
	return t.UTC()
}

// series makes one metric over consecutive days ending at last, oldest first.
func series(name, last string, vals ...float64) []Metric {
	var out []Metric
	t, _ := time.ParseInLocation("2006-01-02", last, dIST)
	for i, v := range vals {
		d := t.AddDate(0, 0, -(len(vals) - 1 - i)).Format("2006-01-02")
		out = append(out, Metric{Time: dgDay(d), Name: name, Value: v})
	}
	return out
}

// steady is n days alternating around mean, so there is a spread of about 1.
func steady(n int, mean float64) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = mean + float64(i%3-1)
	}
	return v
}

func dgFind(d Digest, metric string) DigestMetric {
	for _, m := range d.Metrics {
		if m.Metric == metric {
			return m
		}
	}
	return DigestMetric{}
}

func TestHRVWellBelowTheOwnersNormalIsMarkedAndWorse(t *testing.T) {
	vals := append(steady(30, 50), 30) // 30 usual days then a low one
	d := BuildDigest(series("hrv_last_night_avg", "2026-11-23", vals...), nil, nil, "", dIST, dNow)
	m := dgFind(d, "hrv_last_night_avg")
	if !m.Judged || m.Direction != "below" || m.Strength != "marked" || m.Reading != "worse" {
		t.Fatalf("%+v", m)
	}
	if m.Baseline == nil || m.Baseline.Days != 30 || math.Abs(m.Baseline.Mean-50) > 0.5 {
		t.Fatalf("baseline %+v", m.Baseline)
	}
	if m.Display != "30 ms" || m.Baseline.MeanDisplay != "50 ms" {
		t.Fatalf("display %q / %q", m.Display, m.Baseline.MeanDisplay)
	}
	if len(d.Notable) != 1 || d.Notable[0].Metric != "hrv_last_night_avg" || d.Notable[0].Usual != "50 ms" {
		t.Fatalf("notable %+v", d.Notable)
	}
}

func TestTheSameDeviationIsBetterWhenLowerIsGood(t *testing.T) {
	vals := append(steady(30, 60), 50) // resting heart rate far below usual
	m := dgFind(BuildDigest(series("hr_resting", "2026-11-23", vals...), nil, nil, "", dIST, dNow), "hr_resting")
	if m.Direction != "below" || m.Reading != "better" {
		t.Fatalf("%+v", m)
	}
}

func TestAValueWithinOneSpreadIsTypicalAndNotNotable(t *testing.T) {
	vals := append(steady(30, 50), 50)
	d := BuildDigest(series("hrv_last_night_avg", "2026-11-23", vals...), nil, nil, "", dIST, dNow)
	m := dgFind(d, "hrv_last_night_avg")
	if m.Strength != "typical" || m.Reading != "typical" || len(d.Notable) != 0 {
		t.Fatalf("%+v %+v", m, d.Notable)
	}
}

func TestContextMetricsAreNeverCalledBetterOrWorse(t *testing.T) {
	vals := append(steady(30, 4000), 9000)
	m := dgFind(BuildDigest(series("sleep_deep_seconds", "2026-11-23", vals...), nil, nil, "", dIST, dNow), "sleep_deep_seconds")
	if m.Strength != "marked" || m.Reading != "" {
		t.Fatalf("deep sleep has no good direction: %+v", m)
	}
}

func TestTooFewBaselineDaysMakesNoJudgement(t *testing.T) {
	vals := append(steady(5, 50), 20)
	d := BuildDigest(series("hrv_last_night_avg", "2026-11-23", vals...), nil, nil, "", dIST, dNow)
	m := dgFind(d, "hrv_last_night_avg")
	if m.Judged || m.Reading != "" || m.Direction != "" || !strings.Contains(m.Why, "baseline days") || len(d.Notable) != 0 {
		t.Fatalf("5 days of history must not support a verdict: %+v", m)
	}
	if m.Value == nil || m.Display != "20 ms" {
		t.Fatalf("the value itself is still reported: %+v", m)
	}
}

func TestAPartDayFigureIsNotJudgedButAnOvernightOneIs(t *testing.T) {
	ms := append(series("steps", "2026-11-23", append(steady(30, 8000), 900)...),
		series("hrv_last_night_avg", "2026-11-23", append(steady(30, 50), 30)...)...)
	d := BuildDigest(ms, nil, nil, "", dIST, dNow)
	if !d.DayInProgress {
		t.Fatal("07:15 on the 23rd is the 23rd in progress")
	}
	if s := dgFind(d, "steps"); s.Judged || !strings.Contains(s.Why, "in progress") || s.Trend != "" || s.Last7Mean != nil {
		t.Fatalf("900 steps at 07:15 is not a low-activity day: %+v", s)
	}
	if h := dgFind(d, "hrv_last_night_avg"); !h.Judged {
		t.Fatalf("overnight HRV is complete by morning: %+v", h)
	}
	if d2 := BuildDigest(ms, nil, nil, "2026-11-22", dIST, dNow); d2.DayInProgress || !dgFind(d2, "steps").Judged {
		t.Fatalf("yesterday is a complete day")
	}
}

func TestNoDataForTodayFallsBackToTheNewestDayAndSaysSo(t *testing.T) {
	d := BuildDigest(series("hr_resting", "2026-11-21", steady(35, 58)...), nil, nil, "", dIST, dNow)
	if d.Date != "2026-11-21" || !strings.Contains(d.Note, "2026-11-21") || d.DaysBehind != 2 || d.Stale {
		t.Fatalf("%+v", d)
	}
	d = BuildDigest(series("hr_resting", "2026-11-19", steady(35, 58)...), nil, nil, "", dIST, dNow)
	if d.DaysBehind != 4 || !d.Stale {
		t.Fatalf("4 days behind is stale: %+v", d)
	}
}

func TestNoDataAtAllIsStaleWithANoteAndNoInventedValues(t *testing.T) {
	d := BuildDigest(nil, nil, nil, "", dIST, dNow)
	if !d.Stale || d.Note == "" || len(d.Notable) != 0 {
		t.Fatalf("%+v", d)
	}
	for _, m := range d.Metrics {
		if m.Value != nil || m.Judged {
			t.Fatalf("%s has a value from nothing", m.Metric)
		}
	}
}

func TestAnExplicitDayWithNoDataSaysSo(t *testing.T) {
	d := BuildDigest(series("hr_resting", "2026-11-23", steady(35, 58)...), nil, nil, "2026-10-01", dIST, dNow)
	if d.Date != "2026-10-01" || !strings.Contains(d.Note, "no health data for 2026-10-01") {
		t.Fatalf("%+v", d)
	}
}

func TestMissingOvernightMetricsAreListedButNotPartDayOnes(t *testing.T) {
	d := BuildDigest(series("hr_resting", "2026-11-23", steady(35, 58)...), nil, nil, "", dIST, dNow)
	missing := strings.Join(d.Missing, ",")
	if !strings.Contains(missing, "sleep_total_seconds") || strings.Contains(missing, "steps") || strings.Contains(missing, "stress_avg") {
		t.Fatalf("missing: %s", missing)
	}
}

func TestWeekOnWeekTrendNamesTheDirectionByWhatIsGood(t *testing.T) {
	// 23 days around 55, then 7 days around 45: HRV is lower than the week before.
	vals := append(steady(23, 55), steady(7, 45)...)
	m := dgFind(BuildDigest(series("hrv_last_night_avg", "2026-11-23", vals...), nil, nil, "", dIST, dNow), "hrv_last_night_avg")
	if m.Trend != "worsening" || m.Last7Mean == nil || m.Prev7Mean == nil || *m.Last7Mean >= *m.Prev7Mean {
		t.Fatalf("%+v", m)
	}
	flat := dgFind(BuildDigest(series("hrv_last_night_avg", "2026-11-23", steady(31, 55)...), nil, nil, "", dIST, dNow), "hrv_last_night_avg")
	if flat.Trend != "flat" {
		t.Fatalf("%+v", flat)
	}
}

func TestNotableIsOrderedByHowFarFromUsual(t *testing.T) {
	ms := append(series("hrv_last_night_avg", "2026-11-23", append(steady(30, 50), 46)...), // about 4 spreads? no: tiny spread, so large z too
		series("sleep_score", "2026-11-23", append(steady(30, 80), 90)...)...)
	d := BuildDigest(ms, nil, nil, "", dIST, dNow)
	if len(d.Notable) != 2 {
		t.Fatalf("%+v", d.Notable)
	}
	za, zb := math.Abs(*dgFind(d, d.Notable[0].Metric).ZScore), math.Abs(*dgFind(d, d.Notable[1].Metric).ZScore)
	if za < zb {
		t.Fatalf("not ordered: %v then %v", za, zb)
	}
}

func TestActivityLoad(t *testing.T) {
	at := func(d string, mins float64, typ string) Activity { return Activity{Time: dgDay(d).Add(10 * time.Hour), Type: typ, DurationSeconds: mins * 60} }
	acts := []Activity{at("2026-11-23", 30, "walking"), at("2026-11-21", 60, "strength_training"), at("2026-11-10", 40, "running"), at("2026-10-01", 90, "cycling")}
	d := BuildDigest(series("hr_resting", "2026-11-23", steady(35, 58)...), acts, nil, "", dIST, dNow)
	a := d.Activity
	if a.Last7Days.Count != 2 || a.Last7Days.TotalMinutes != 90 || a.Last7Days.ByType["strength_training"] != 1 {
		t.Fatalf("7 days: %+v", a.Last7Days)
	}
	if a.WeeklyMinutes28 != 32.5 { // (30+60+40)/4: the October ride is outside 28 days
		t.Fatalf("28-day weekly average %v", a.WeeklyMinutes28)
	}
	if len(a.OnTheDay) != 1 || a.OnTheDay[0].Type != "walking" || a.Last == nil || *a.DaysSinceLast != 0 {
		t.Fatalf("%+v", a)
	}
	d = BuildDigest(series("hr_resting", "2026-11-23", steady(35, 58)...), acts[1:], nil, "", dIST, dNow)
	if *d.Activity.DaysSinceLast != 2 || len(d.Activity.OnTheDay) != 0 {
		t.Fatalf("%+v", d.Activity)
	}
}

func TestDisplayNumbers(t *testing.T) {
	for _, c := range []struct {
		unit string
		v    float64
		want string
	}{
		{"seconds", 27720, "7h 42m"}, {"seconds", 27719, "7h 42m"}, {"seconds", 3599, "1h 00m"}, {"seconds", 840, "14m"},
		{"bpm", 57.4, "57 bpm"}, {"ms", 37, "37 ms"}, {"steps", 2266, "2,266"}, {"steps", 1234567, "1,234,567"},
		{"meters", 1784, "1.8 km"}, {"0-100", 82, "82"}, {"kcal", 1975, "1975 kcal"},
	} {
		if got := display(c.unit, c.v); got != c.want {
			t.Errorf("display(%s,%v) = %q, want %q", c.unit, c.v, got, c.want)
		}
	}
}
