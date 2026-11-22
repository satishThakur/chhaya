package health

import (
	"sort"
	"strings"
	"time"
)

// DayValue is one metric on one local calendar day.
type DayValue struct {
	Day   string  `json:"day"`
	Value float64 `json:"value"`
}

// MetricSummary is what one metric did over the window: the quantities, not a verdict. The
// earlier and later means are the first and second half of the days that had data, shown only
// from six days up, so the reader sees both numbers instead of a percentage with a confidence
// nobody computed.
type MetricSummary struct {
	Metric      string   `json:"metric"`
	Unit        string   `json:"unit"`
	Days        int      `json:"days_with_data"`
	Mean        float64  `json:"mean"`
	Min         float64  `json:"min"`
	Max         float64  `json:"max"`
	Latest      DayValue `json:"latest"`
	EarlierMean *float64 `json:"earlier_half_mean,omitempty"`
	LaterMean   *float64 `json:"later_half_mean,omitempty"`
}

// ActivitySummary totals the workouts in the window.
type ActivitySummary struct {
	Count        int            `json:"count"`
	TotalMinutes float64        `json:"total_minutes"`
	ByType       map[string]int `json:"by_type,omitempty"`
}

// SyncStatus is the sidecar's last run.
type SyncStatus struct {
	Status   string   `json:"status"`
	HoursAgo int      `json:"hours_ago"`
	Errors   []string `json:"errors,omitempty"`
}

// Summary is the answer to health.get_summary.
type Summary struct {
	Days       int             `json:"window_days"`
	WithData   int             `json:"days_with_any_data"`
	LatestDay  string          `json:"latest_day_with_data,omitempty"`
	DaysBehind int             `json:"days_behind"`
	Stale      bool            `json:"stale"`
	Note       string          `json:"note,omitempty"`
	Metrics    []MetricSummary `json:"metrics"`
	Activities ActivitySummary `json:"activities"`
	LastSync   *SyncStatus     `json:"last_sync,omitempty"`
}

// staleAfterDays is how far behind the newest data may be before the summary says it is stale.
const staleAfterDays = 2

func localDay(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02") }

// Summarize reduces a window of rows. It reads no clock and no database.
func Summarize(ms []Metric, acts []Activity, run *Run, days int, loc *time.Location, now time.Time) Summary {
	s := Summary{Days: days, Metrics: []MetricSummary{}, Activities: ActivitySummary{ByType: map[string]int{}}}
	by := map[string][]DayValue{}
	daysSeen := map[string]bool{}
	for _, m := range ms {
		d := localDay(m.Time, loc)
		by[m.Name] = append(by[m.Name], DayValue{d, m.Value})
		daysSeen[d] = true
	}
	var names []string
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := by[n]
		sort.Slice(v, func(i, j int) bool { return v[i].Day < v[j].Day })
		out := MetricSummary{Metric: n, Unit: unitOf(n), Days: len(v), Min: v[0].Value, Max: v[0].Value, Latest: v[len(v)-1]}
		sum := 0.0
		for _, p := range v {
			sum += p.Value
			out.Min, out.Max = min(out.Min, p.Value), max(out.Max, p.Value)
		}
		out.Mean = sum / float64(len(v))
		if len(v) >= 6 {
			h := len(v) / 2
			e, l := mean(v[:h]), mean(v[len(v)-h:])
			out.EarlierMean, out.LaterMean = &e, &l
		}
		s.Metrics = append(s.Metrics, out)
	}
	s.WithData = len(daysSeen)
	if s.WithData > 0 {
		for d := range daysSeen {
			if d > s.LatestDay {
				s.LatestDay = d
			}
		}
		latest, _ := time.ParseInLocation("2006-01-02", s.LatestDay, loc)
		n := now.In(loc)
		today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
		s.DaysBehind = int(today.Sub(latest).Round(24*time.Hour) / (24 * time.Hour))
		if s.DaysBehind < 0 {
			s.DaysBehind = 0
		}
		s.Stale = s.DaysBehind > staleAfterDays
	} else {
		s.Stale = true
		s.Note = "No health data in this window. Either nothing has synced yet or the sync is failing; last_sync says which."
	}
	for _, a := range acts {
		s.Activities.Count++
		s.Activities.TotalMinutes += a.DurationSeconds / 60
		s.Activities.ByType[a.Type]++
	}
	if run != nil {
		s.LastSync = &SyncStatus{Status: run.Status, HoursAgo: int(now.Sub(run.FinishedAt) / time.Hour), Errors: run.Errors}
	}
	return s
}

// SeriesOf is one metric's days, oldest first.
func SeriesOf(ms []Metric, metric string, loc *time.Location) []DayValue {
	var out []DayValue
	for _, m := range ms {
		if m.Name == metric {
			out = append(out, DayValue{localDay(m.Time, loc), m.Value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

func mean(v []DayValue) float64 {
	t := 0.0
	for _, p := range v {
		t += p.Value
	}
	return t / float64(len(v))
}

func unitOf(name string) string {
	switch {
	case strings.HasSuffix(name, "_seconds"):
		return "seconds"
	case strings.HasSuffix(name, "_minutes"):
		return "minutes"
	case strings.HasPrefix(name, "hr_"):
		return "bpm"
	case strings.Contains(name, "hrv"):
		return "ms"
	case strings.HasPrefix(name, "calories"):
		return "kcal"
	case name == "distance_meters":
		return "meters"
	case name == "steps":
		return "steps"
	}
	return "0-100"
}
