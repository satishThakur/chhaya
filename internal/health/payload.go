// Package health is the health module of the gateway: Garmin data arrives from the sync sidecar
// over localhost, is validated here, lands in TimescaleDB, and is read back by the health.* tools.
// docs/health.md.
//
// The payload is typed and strict on purpose. The pipeline this was ported from sent HRV as
// last_night_avg while its validator demanded average_hrv, and every HRV post was refused for
// months with nobody told. Here an unknown field is an error that names it, and a field the
// sidecar did not send is absent, never a 0 in the series.
package health

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	_ "time/tzdata" // the gateway image is alpine with no zoneinfo
)

// maxActivities bounds one day's batch. A person does not log fifty workouts in a day.
const maxActivities = 50

// Day is one calendar day of data, as the sidecar sends it. Every section is optional; a day with
// no section at all is refused.
type Day struct {
	Date        string       `json:"date"`
	Sleep       *Sleep       `json:"sleep,omitempty"`
	HRV         *HRV         `json:"hrv,omitempty"`
	Stress      *Stress      `json:"stress,omitempty"`
	DailyStats  *DailyStats  `json:"daily_stats,omitempty"`
	BodyBattery *BodyBattery `json:"body_battery,omitempty"`
	Activities  []ActivityIn `json:"activities,omitempty"`
}

type Sleep struct {
	TotalSeconds *float64 `json:"total_seconds"`
	DeepSeconds  *float64 `json:"deep_seconds"`
	LightSeconds *float64 `json:"light_seconds"`
	REMSeconds   *float64 `json:"rem_seconds"`
	AwakeSeconds *float64 `json:"awake_seconds"`
	Score        *float64 `json:"score"`
	AvgHRV       *float64 `json:"avg_hrv"`
	EndedAt      *string  `json:"ended_at"` // RFC 3339; checked, not stored
}

type HRV struct {
	LastNightAvg *float64 `json:"last_night_avg"`
	WeeklyAvg    *float64 `json:"weekly_avg"`
}

type Stress struct {
	Avg         *float64 `json:"avg"`
	Max         *float64 `json:"max"`
	RestSeconds *float64 `json:"rest_seconds"`
}

type DailyStats struct {
	Steps                    *float64 `json:"steps"`
	CaloriesTotal            *float64 `json:"calories_total"`
	CaloriesActive           *float64 `json:"calories_active"`
	CaloriesBMR              *float64 `json:"calories_bmr"`
	DistanceMeters           *float64 `json:"distance_meters"`
	HRResting                *float64 `json:"hr_resting"`
	HRMin                    *float64 `json:"hr_min"`
	HRMax                    *float64 `json:"hr_max"`
	IntensityModerateMinutes *float64 `json:"intensity_moderate_minutes"`
	IntensityVigorousMinutes *float64 `json:"intensity_vigorous_minutes"`
}

type BodyBattery struct {
	Charged *float64 `json:"charged"`
	Drained *float64 `json:"drained"`
	High    *float64 `json:"high"`
	Low     *float64 `json:"low"`
}

type ActivityIn struct {
	Type            string   `json:"type"`
	StartTime       string   `json:"start_time"` // RFC 3339
	DurationSeconds float64  `json:"duration_seconds"`
	DistanceMeters  *float64 `json:"distance_meters"`
	Calories        *float64 `json:"calories"`
	AvgHR           *float64 `json:"avg_hr"`
	MaxHR           *float64 `json:"max_hr"`
}

// Metric is one named number on one day. Time is that day's midnight in the configured zone.
type Metric struct {
	Time  time.Time
	Name  string
	Value float64
}

// Activity is one workout.
type Activity struct {
	Time            time.Time
	Type            string
	DurationSeconds float64
	DistanceMeters  *float64
	Calories        *float64
	AvgHR           *float64
	MaxHR           *float64
}

// bounds is the range a metric may take. Garmin does not send values outside these; one that does
// is a parse error upstream, and storing it would poison every average after it.
type bounds struct{ lo, hi float64 }

var catalogue = map[string]bounds{
	"sleep_total_seconds":        {0, 86400},
	"sleep_deep_seconds":         {0, 86400},
	"sleep_light_seconds":        {0, 86400},
	"sleep_rem_seconds":          {0, 86400},
	"sleep_awake_seconds":        {0, 86400},
	"sleep_score":                {0, 100},
	"sleep_avg_hrv":              {0, 500},
	"hrv_last_night_avg":         {0, 500},
	"hrv_weekly_avg":             {0, 500},
	"stress_avg":                 {0, 100},
	"stress_max":                 {0, 100},
	"stress_rest_seconds":        {0, 86400},
	"steps":                      {0, 200000},
	"calories_total":             {0, 20000},
	"calories_active":            {0, 20000},
	"calories_bmr":               {0, 20000},
	"distance_meters":            {0, 500000},
	"hr_resting":                 {20, 250},
	"hr_min":                     {20, 250},
	"hr_max":                     {20, 250},
	"intensity_moderate_minutes": {0, 1440},
	"intensity_vigorous_minutes": {0, 1440},
	"body_battery_charged":       {0, 200},
	"body_battery_drained":       {0, 200},
	"body_battery_high":          {0, 100},
	"body_battery_low":           {0, 100},
}

// Known reports whether name is a metric the store holds.
func Known(name string) bool { _, ok := catalogue[name]; return ok }

// MetricNames lists every metric, for the tools' descriptions.
func MetricNames() []string {
	out := make([]string, 0, len(catalogue))
	for n := range catalogue {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}

// Decode reads one day. Unknown fields and trailing data are errors.
func Decode(r io.Reader) (Day, error) {
	var d Day
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Day{}, fmt.Errorf("body is not a valid day: %w", err)
	}
	if dec.More() {
		return Day{}, errors.New("body holds more than one JSON document")
	}
	var rest bytes.Buffer
	if _, err := io.Copy(&rest, dec.Buffered()); err == nil && len(bytes.TrimSpace(rest.Bytes())) > 0 {
		return Day{}, errors.New("body holds more than one JSON document")
	}
	return d, nil
}

// Rows validates the day and turns it into rows. loc is the zone whose midnight stamps the day;
// now bounds how far ahead a date may be (one day, for clock skew).
func (d Day) Rows(loc *time.Location, now time.Time) ([]Metric, []Activity, error) {
	if d.Date == "" {
		return nil, nil, errors.New("date is required")
	}
	day, err := time.ParseInLocation("2006-01-02", d.Date, loc)
	if err != nil {
		return nil, nil, errors.New("date must be YYYY-MM-DD")
	}
	today := now.In(loc)
	latest := time.Date(today.Year(), today.Month(), today.Day()+1, 0, 0, 0, 0, loc)
	if day.After(latest) {
		return nil, nil, fmt.Errorf("date %s is more than a day ahead", d.Date)
	}
	if len(d.Activities) > maxActivities {
		return nil, nil, fmt.Errorf("%d activities in one day; the limit is %d", len(d.Activities), maxActivities)
	}

	var ms []Metric
	var firstErr error
	add := func(name string, v *float64) {
		if v == nil || firstErr != nil {
			return
		}
		b := catalogue[name]
		if *v < b.lo || *v > b.hi || *v != *v {
			firstErr = fmt.Errorf("%s = %v is outside %v to %v", name, *v, b.lo, b.hi)
			return
		}
		ms = append(ms, Metric{Time: day.UTC(), Name: name, Value: *v})
	}
	if s := d.Sleep; s != nil {
		add("sleep_total_seconds", s.TotalSeconds)
		add("sleep_deep_seconds", s.DeepSeconds)
		add("sleep_light_seconds", s.LightSeconds)
		add("sleep_rem_seconds", s.REMSeconds)
		add("sleep_awake_seconds", s.AwakeSeconds)
		add("sleep_score", s.Score)
		add("sleep_avg_hrv", s.AvgHRV)
		if s.EndedAt != nil {
			if _, err := time.Parse(time.RFC3339, *s.EndedAt); err != nil {
				return nil, nil, errors.New("sleep.ended_at must be RFC 3339")
			}
		}
	}
	if h := d.HRV; h != nil {
		add("hrv_last_night_avg", h.LastNightAvg)
		add("hrv_weekly_avg", h.WeeklyAvg)
	}
	if s := d.Stress; s != nil {
		add("stress_avg", s.Avg)
		add("stress_max", s.Max)
		add("stress_rest_seconds", s.RestSeconds)
	}
	if s := d.DailyStats; s != nil {
		add("steps", s.Steps)
		add("calories_total", s.CaloriesTotal)
		add("calories_active", s.CaloriesActive)
		add("calories_bmr", s.CaloriesBMR)
		add("distance_meters", s.DistanceMeters)
		add("hr_resting", s.HRResting)
		add("hr_min", s.HRMin)
		add("hr_max", s.HRMax)
		add("intensity_moderate_minutes", s.IntensityModerateMinutes)
		add("intensity_vigorous_minutes", s.IntensityVigorousMinutes)
	}
	if b := d.BodyBattery; b != nil {
		add("body_battery_charged", b.Charged)
		add("body_battery_drained", b.Drained)
		add("body_battery_high", b.High)
		add("body_battery_low", b.Low)
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}

	var acts []Activity
	for i, a := range d.Activities {
		if a.Type == "" {
			return nil, nil, fmt.Errorf("activities[%d].type is required", i)
		}
		start, err := time.Parse(time.RFC3339, a.StartTime)
		if err != nil {
			return nil, nil, fmt.Errorf("activities[%d].start_time must be RFC 3339", i)
		}
		if !(a.DurationSeconds > 0 && a.DurationSeconds <= 86400) {
			return nil, nil, fmt.Errorf("activities[%d].duration_seconds must be between 0 and a day", i)
		}
		for name, v := range map[string]*float64{"distance_meters": a.DistanceMeters, "calories": a.Calories} {
			if v != nil && (*v < 0 || *v > 1e6) {
				return nil, nil, fmt.Errorf("activities[%d].%s is out of range", i, name)
			}
		}
		for name, v := range map[string]*float64{"avg_hr": a.AvgHR, "max_hr": a.MaxHR} {
			if v != nil && (*v < 20 || *v > 250) {
				return nil, nil, fmt.Errorf("activities[%d].%s is out of range", i, name)
			}
		}
		acts = append(acts, Activity{Time: start.UTC(), Type: a.Type, DurationSeconds: a.DurationSeconds,
			DistanceMeters: a.DistanceMeters, Calories: a.Calories, AvgHR: a.AvgHR, MaxHR: a.MaxHR})
	}
	if len(ms) == 0 && len(acts) == 0 {
		return nil, nil, errors.New("the day holds no data")
	}
	return ms, acts, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
