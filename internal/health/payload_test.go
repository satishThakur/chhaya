package health

import (
	"sort"
	"strings"
	"testing"
	"time"
)

var ist = mustLoc("Asia/Kolkata")

func mustLoc(n string) *time.Location {
	l, err := time.LoadLocation(n)
	if err != nil {
		panic(err)
	}
	return l
}

var now = time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)

const fullDay = `{
 "date": "2026-11-21",
 "sleep": {"total_seconds": 27000, "deep_seconds": 5400, "light_seconds": 14400, "rem_seconds": 5400, "awake_seconds": 1800, "score": 81, "avg_hrv": 52, "ended_at": "2026-11-21T00:40:00Z"},
 "hrv": {"last_night_avg": 51, "weekly_avg": 49},
 "stress": {"avg": 28, "max": 91, "rest_seconds": 21000},
 "daily_stats": {"steps": 9120, "calories_total": 2410, "calories_active": 610, "calories_bmr": 1800, "distance_meters": 7100, "hr_resting": 54, "hr_min": 47, "hr_max": 151, "intensity_moderate_minutes": 22, "intensity_vigorous_minutes": 8},
 "body_battery": {"charged": 62, "drained": 58, "high": 91, "low": 17},
 "activities": [{"type": "running", "start_time": "2026-11-21T00:30:00Z", "duration_seconds": 3000, "distance_meters": 10000, "calories": 640, "avg_hr": 148, "max_hr": 171}]
}`

func decode(t *testing.T, s string) (Day, error) {
	t.Helper()
	return Decode(strings.NewReader(s))
}

func names(ms []Metric) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

func TestAFullDayBecomesNamedMetricsAtMidnightInTheConfiguredZone(t *testing.T) {
	d, err := decode(t, fullDay)
	if err != nil {
		t.Fatal(err)
	}
	ms, acts, err := d.Rows(ist, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 7+2+3+10+4 || len(acts) != 1 {
		t.Fatalf("%d metrics %d activities: %v", len(ms), len(acts), names(ms))
	}
	want := time.Date(2026, 11, 20, 18, 30, 0, 0, time.UTC) // 2026-11-21 00:00 IST
	for _, m := range ms {
		if !m.Time.Equal(want) {
			t.Fatalf("%s at %v, want %v", m.Name, m.Time, want)
		}
	}
	got := map[string]float64{}
	for _, m := range ms {
		got[m.Name] = m.Value
	}
	if got["sleep_total_seconds"] != 27000 || got["hrv_last_night_avg"] != 51 || got["steps"] != 9120 || got["body_battery_high"] != 91 || got["stress_avg"] != 28 {
		t.Fatalf("values: %v", got)
	}
	if acts[0].Type != "running" || acts[0].DurationSeconds != 3000 || acts[0].AvgHR == nil || *acts[0].AvgHR != 148 {
		t.Fatalf("activity: %+v", acts[0])
	}
}

func TestAMissingFieldIsAbsentNotZero(t *testing.T) {
	d, err := decode(t, `{"date":"2026-11-21","sleep":{"total_seconds":25000},"daily_stats":{"steps":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	ms, _, err := d.Rows(ist, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(ms), ","); got != "sleep_total_seconds,steps" {
		t.Fatalf("metrics: %s (a field the sidecar did not send must not become a 0 in the series)", got)
	}
	for _, m := range ms {
		if m.Name == "steps" && m.Value != 0 {
			t.Fatal("an explicit 0 must stay 0")
		}
	}
}

func TestAFieldTheStoreDoesNotKnowIsRefusedByName(t *testing.T) {
	// The old pipeline sent last_night_avg while its validator wanted average_hrv, and every HRV
	// post was rejected for months. A strict decoder turns a mismatch into an error naming the field.
	for _, body := range []string{
		`{"date":"2026-11-21","hrv":{"average_hrv":50}}`,
		`{"date":"2026-11-21","extra":1}`,
		`{"date":"2026-11-21","activities":[{"type":"run","start_time":"2026-11-21T00:30:00Z","duration_seconds":60,"mood":"good"}]}`,
	} {
		if _, err := decode(t, body); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestOutOfRangeAndNonsenseAreRefusedAndNamed(t *testing.T) {
	cases := map[string]string{
		"stress over 100":     `{"date":"2026-11-21","stress":{"avg":101}}`,
		"negative steps":      `{"date":"2026-11-21","daily_stats":{"steps":-1}}`,
		"hr above 250":        `{"date":"2026-11-21","daily_stats":{"hr_max":300}}`,
		"sleep over a day":    `{"date":"2026-11-21","sleep":{"total_seconds":90000}}`,
		"score over 100":      `{"date":"2026-11-21","sleep":{"score":120}}`,
		"battery over 100":    `{"date":"2026-11-21","body_battery":{"high":120}}`,
		"no activity type":    `{"date":"2026-11-21","activities":[{"start_time":"2026-11-21T00:30:00Z","duration_seconds":60}]}`,
		"zero duration":       `{"date":"2026-11-21","activities":[{"type":"run","start_time":"2026-11-21T00:30:00Z","duration_seconds":0}]}`,
		"no start":            `{"date":"2026-11-21","activities":[{"type":"run","duration_seconds":60}]}`,
		"start not RFC3339":   `{"date":"2026-11-21","activities":[{"type":"run","start_time":"yesterday","duration_seconds":60}]}`,
		"sleep end not RFC":   `{"date":"2026-11-21","sleep":{"total_seconds":100,"ended_at":"soon"}}`,
		"bad date":            `{"date":"21/11/2026","sleep":{"total_seconds":100}}`,
		"no date":             `{"sleep":{"total_seconds":100}}`,
		"nothing in the day":  `{"date":"2026-11-21"}`,
		"empty sections only": `{"date":"2026-11-21","sleep":{},"activities":[]}`,
		"far future":          `{"date":"2026-12-30","sleep":{"total_seconds":100}}`,
		"too many activities": `{"date":"2026-11-21","activities":[` + strings.Repeat(`{"type":"run","start_time":"2026-11-21T00:30:00Z","duration_seconds":60},`, 60) + `{"type":"run","start_time":"2026-11-21T00:30:00Z","duration_seconds":60}]}`,
	}
	for name, body := range cases {
		d, err := decode(t, body)
		if err == nil {
			_, _, err = d.Rows(ist, now)
		}
		if err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestTodayAndYesterdayAreAccepted(t *testing.T) {
	for _, day := range []string{"2026-11-22", "2026-11-23", "2026-11-01"} { // the new IST day, one ahead for a clock skew, an old backfill
		d, err := decode(t, `{"date":"`+day+`","sleep":{"total_seconds":100}}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.Rows(ist, now); err != nil {
			t.Errorf("%s: %v", day, err)
		}
	}
}

func TestTrailingJSONAfterTheBodyIsRefused(t *testing.T) {
	if _, err := decode(t, `{"date":"2026-11-21","sleep":{"total_seconds":100}} {"x":1}`); err == nil {
		t.Fatal("two documents in one body")
	}
}
