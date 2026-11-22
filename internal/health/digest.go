package health

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// What the morning digest needs is not the raw series but the owner's own numbers set against
// the owner's own recent normal, worked out once, here, in code. The model that writes the digest
// (and Hermes, when asked in chat) is handed these facts and may not recompute them: a model asked
// to "compare last night with the average" gets it subtly different each run.
//
// Every judgement is relative to the owner's previous 30 days, never to a population norm, and is
// made only with enough days to mean something. docs/health.md, "The digest tool".

const (
	baselineDays = 30 // the days before the one described
	minBaseline  = 14 // fewer days than this and no judgement is made
	trendDays    = 7
	minTrendDays = 4 // days each side of a week-on-week comparison
	sdFloorShare = 0.03
	maxNotable   = 6
)

// Method is stated in the tool's answer so whoever reads it can say how a word was decided.
const Method = "Each judgement compares one value with the owner's own previous 30 days: typical is within 1 standard " +
	"deviation of their mean, notable is 1 to 2, marked is 2 or more; reading says whether that direction is better or worse " +
	"for the metric. Nothing is judged with fewer than 14 baseline days. trend compares the last 7 days with the 7 before " +
	"and is flat when the change is under half a standard deviation. Not a diagnosis."

// digestSpec says how one metric is judged. good is the favourable direction: "higher", "lower", or
// "" for a metric that is context, not a score (deep sleep and steps are neither good nor bad
// alone). overnight metrics are complete by morning; the others are still accumulating if the day
// described is today.
var digestSpec = []struct {
	name, good string
	overnight  bool
}{
	{"sleep_total_seconds", "higher", true},
	{"sleep_score", "higher", true},
	{"sleep_deep_seconds", "", true},
	{"sleep_rem_seconds", "", true},
	{"sleep_awake_seconds", "lower", true},
	{"hrv_last_night_avg", "higher", true},
	{"hr_resting", "lower", true},
	{"body_battery_charged", "higher", true},
	{"stress_avg", "lower", false},
	{"body_battery_high", "higher", false},
	{"steps", "", false},
}

// Baseline is the owner's previous 30 days of one metric.
type Baseline struct {
	Days        int     `json:"days_with_data"`
	Mean        float64 `json:"mean"`
	MeanDisplay string  `json:"mean_display"`
	SD          float64 `json:"std_dev"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
}

// DigestMetric is one metric on the day described.
type DigestMetric struct {
	Metric   string    `json:"metric"`
	Value    *float64  `json:"value,omitempty"`
	Display  string    `json:"display,omitempty"`
	Baseline *Baseline `json:"baseline,omitempty"`
	Delta    *float64  `json:"delta_from_mean,omitempty"`
	DeltaPct *float64  `json:"delta_pct,omitempty"`
	ZScore   *float64  `json:"z_score,omitempty"`
	// Direction is above, below or typical; Strength is typical, notable or marked; Reading is
	// better, worse or typical (absent for a metric that is context, not a score); Judged says
	// whether any of it was decided, and Why says what stopped it when not.
	Direction string `json:"direction,omitempty"`
	Strength  string `json:"strength,omitempty"`
	Reading   string `json:"reading,omitempty"`
	Judged    bool   `json:"judged"`
	Why       string `json:"why_not_judged,omitempty"`
	// Last 7 days against the 7 before, when each side has 4 or more days.
	Last7Mean *float64 `json:"last_7_days_mean,omitempty"`
	Prev7Mean *float64 `json:"previous_7_days_mean,omitempty"`
	Trend     string   `json:"trend,omitempty"` // improving, worsening, rising, falling, flat
}

// Finding is a judged metric that is not typical, in the order a reader should care.
type Finding struct {
	Metric    string `json:"metric"`
	Direction string `json:"direction"`
	Strength  string `json:"strength"`
	Reading   string `json:"reading,omitempty"`
	Display   string `json:"display"`
	Usual     string `json:"usual"`
}

// DigestActivity is the training load around the day described.
type DigestActivity struct {
	Last7Days       ActivitySummary `json:"last_7_days"`
	WeeklyMinutes28 float64         `json:"average_weekly_minutes_last_28_days"`
	OnTheDay        []DigestWorkout `json:"on_the_day,omitempty"`
	Last            *DigestWorkout  `json:"most_recent,omitempty"`
	DaysSinceLast   *int            `json:"days_since_most_recent,omitempty"`
}

// DigestWorkout is one workout.
type DigestWorkout struct {
	Day     string   `json:"day"`
	Type    string   `json:"type"`
	Minutes float64  `json:"minutes"`
	AvgHR   *float64 `json:"avg_hr,omitempty"`
}

// Digest is the answer to health.get_digest.
type Digest struct {
	Date          string         `json:"date"`
	DayInProgress bool           `json:"day_in_progress"`
	Note          string         `json:"note,omitempty"`
	LatestDay     string         `json:"latest_day_with_data,omitempty"`
	DaysBehind    int            `json:"days_behind"`
	Stale         bool           `json:"stale"`
	LastSync      *SyncStatus    `json:"last_sync,omitempty"`
	Method        string         `json:"method"`
	Metrics       []DigestMetric `json:"metrics"`
	Notable       []Finding      `json:"notable"`
	Missing       []string       `json:"missing_metrics,omitempty"`
	Activity      DigestActivity `json:"activity"`
}

// DigestWindowBack is how many days before the one described the reader must supply: the baseline,
// plus the week-on-week comparison's earlier week.
const DigestWindowBack = baselineDays + 14

// BuildDigest works out the digest for one local day. date is "" for the latest, which is today if
// it has data and otherwise the newest day that does (the note says so). ms and acts must cover at
// least DigestWindowBack days before that day. It reads no clock and no database.
func BuildDigest(ms []Metric, acts []Activity, run *Run, date string, loc *time.Location, now time.Time) Digest {
	d := Digest{Method: Method, Metrics: []DigestMetric{}, Notable: []Finding{}}
	today := localDay(now, loc)

	vals := map[string]map[string]float64{} // metric -> day -> value
	days := map[string]bool{}
	for _, m := range ms {
		day := localDay(m.Time, loc)
		if vals[m.Name] == nil {
			vals[m.Name] = map[string]float64{}
		}
		vals[m.Name][day] = m.Value
		days[day] = true
	}
	for day := range days {
		if day > d.LatestDay && day <= today {
			d.LatestDay = day
		}
	}

	d.Date = date
	switch {
	case date == "" && days[today]:
		d.Date = today
	case date == "" && d.LatestDay != "":
		d.Date = d.LatestDay
		d.Note = "Nothing has synced for today yet, so this describes " + d.LatestDay + ", the newest day with data."
	case date == "":
		d.Date = today
		d.Note = "There is no health data in the window. Either nothing has synced yet or the sync is failing; last_sync says which."
	case !days[date]:
		d.Note = "There is no health data for " + date + "."
	}
	d.DayInProgress = d.Date == today

	if d.LatestDay != "" {
		if l, err := time.ParseInLocation("2006-01-02", d.LatestDay, loc); err == nil {
			if t, err := time.ParseInLocation("2006-01-02", today, loc); err == nil {
				d.DaysBehind = max(0, int(math.Round(t.Sub(l).Hours()/24)))
			}
		}
	}
	d.Stale = d.LatestDay == "" || d.DaysBehind > staleAfterDays
	if run != nil {
		d.LastSync = &SyncStatus{Status: run.Status, HoursAgo: int(now.Sub(run.FinishedAt) / time.Hour), Errors: run.Errors}
	}

	for _, sp := range digestSpec {
		dm := judge(sp.name, sp.good, sp.overnight, vals[sp.name], d.Date, d.DayInProgress, loc)
		if dm.Value == nil && (!d.DayInProgress || sp.overnight) {
			d.Missing = append(d.Missing, sp.name)
		}
		d.Metrics = append(d.Metrics, dm)
	}
	for _, dm := range d.Metrics {
		if dm.Judged && dm.Strength != "typical" {
			d.Notable = append(d.Notable, Finding{Metric: dm.Metric, Direction: dm.Direction, Strength: dm.Strength,
				Reading: dm.Reading, Display: dm.Display, Usual: dm.Baseline.MeanDisplay})
		}
	}
	z := func(metric string) float64 {
		for _, dm := range d.Metrics {
			if dm.Metric == metric && dm.ZScore != nil {
				return math.Abs(*dm.ZScore)
			}
		}
		return 0
	}
	sort.SliceStable(d.Notable, func(i, j int) bool { return z(d.Notable[i].Metric) > z(d.Notable[j].Metric) })
	if len(d.Notable) > maxNotable {
		d.Notable = d.Notable[:maxNotable]
	}
	d.Activity = activityAround(acts, d.Date, loc)
	return d
}

func addDays(day string, n int, loc *time.Location) string {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// judge works out one metric: its value on the day, how that stands against the owner's previous
// 30 days, and the week-on-week trend.
func judge(name, good string, overnight bool, series map[string]float64, day string, inProgress bool, loc *time.Location) DigestMetric {
	dm := DigestMetric{Metric: name}
	unit := unitOf(name)
	if v, ok := series[day]; ok {
		dm.Value, dm.Display = ptr(round2(v)), display(unit, v)
	}
	var base []float64
	for i := 1; i <= baselineDays; i++ {
		if v, ok := series[addDays(day, -i, loc)]; ok {
			base = append(base, v)
		}
	}
	var sd float64
	if len(base) > 0 {
		mu, s := meanSD(base)
		sd = math.Max(s, sdFloorShare*math.Abs(mu))
		mn, mx := base[0], base[0]
		for _, v := range base {
			mn, mx = math.Min(mn, v), math.Max(mx, v)
		}
		dm.Baseline = &Baseline{Days: len(base), Mean: round2(mu), MeanDisplay: display(unit, mu), SD: round2(sd), Min: round2(mn), Max: round2(mx)}
	}
	switch {
	case dm.Value == nil:
		dm.Why = "no value for this day"
	case inProgress && !overnight:
		dm.Why = "the day is still in progress, so this is a part-day figure"
	case len(base) < minBaseline:
		dm.Why = fmt.Sprintf("only %d baseline days; at least %d are needed", len(base), minBaseline)
	case sd == 0:
		dm.Why = "the baseline has no spread to judge against"
	default:
		dm.Judged = true
		mu := dm.Baseline.Mean
		delta := *dm.Value - mu
		zs := delta / sd
		dm.Delta, dm.DeltaPct, dm.ZScore = ptr(round2(delta)), nil, ptr(round2(zs))
		if mu != 0 {
			dm.DeltaPct = ptr(round1(delta / math.Abs(mu) * 100))
		}
		az := math.Abs(zs)
		switch {
		case az < 1:
			dm.Direction, dm.Strength = "typical", "typical"
		case az < 2:
			dm.Strength = "notable"
		default:
			dm.Strength = "marked"
		}
		if dm.Strength != "typical" {
			dm.Direction = map[bool]string{true: "above", false: "below"}[delta > 0]
		}
		if good != "" {
			switch {
			case dm.Strength == "typical":
				dm.Reading = "typical"
			case (dm.Direction == "above") == (good == "higher"):
				dm.Reading = "better"
			default:
				dm.Reading = "worse"
			}
		}
	}
	// Week on week: needs enough days on each side and a spread to compare against. A part-day
	// figure would drag the recent week down, so an accumulating metric skips it on today.
	if dm.Baseline != nil && !(inProgress && !overnight) {
		var last, prev []float64
		for i := 0; i < trendDays; i++ {
			if v, ok := series[addDays(day, -i, loc)]; ok {
				last = append(last, v)
			}
			if v, ok := series[addDays(day, -trendDays-i, loc)]; ok {
				prev = append(prev, v)
			}
		}
		if len(last) >= minTrendDays && len(prev) >= minTrendDays && sd > 0 {
			l, _ := meanSD(last)
			p, _ := meanSD(prev)
			dm.Last7Mean, dm.Prev7Mean = ptr(round2(l)), ptr(round2(p))
			switch {
			case math.Abs(l-p) < 0.5*sd:
				dm.Trend = "flat"
			case good == "":
				dm.Trend = map[bool]string{true: "rising", false: "falling"}[l > p]
			case (l > p) == (good == "higher"):
				dm.Trend = "improving"
			default:
				dm.Trend = "worsening"
			}
		}
	}
	return dm
}

func activityAround(acts []Activity, day string, loc *time.Location) DigestActivity {
	a := DigestActivity{Last7Days: ActivitySummary{ByType: map[string]int{}}}
	from7, from28 := addDays(day, -6, loc), addDays(day, -27, loc)
	var total28 float64
	var last *Activity
	for i := range acts {
		x := acts[i]
		d := localDay(x.Time, loc)
		if d > day {
			continue
		}
		if d >= from28 {
			total28 += x.DurationSeconds / 60
		}
		if d >= from7 {
			a.Last7Days.Count++
			a.Last7Days.TotalMinutes += x.DurationSeconds / 60
			a.Last7Days.ByType[x.Type]++
		}
		if d == day {
			a.OnTheDay = append(a.OnTheDay, workout(x, d))
		}
		if last == nil || x.Time.After(last.Time) {
			last = &acts[i]
		}
	}
	a.Last7Days.TotalMinutes = round1(a.Last7Days.TotalMinutes)
	a.WeeklyMinutes28 = round1(total28 / 4)
	if last != nil {
		w := workout(*last, localDay(last.Time, loc))
		a.Last = &w
		if t, err := time.ParseInLocation("2006-01-02", day, loc); err == nil {
			if l, err := time.ParseInLocation("2006-01-02", w.Day, loc); err == nil {
				a.DaysSinceLast = ptr(int(math.Round(t.Sub(l).Hours() / 24)))
			}
		}
	}
	return a
}

func workout(a Activity, day string) DigestWorkout {
	w := DigestWorkout{Day: day, Type: a.Type, Minutes: round1(a.DurationSeconds / 60)}
	if a.AvgHR != nil {
		w.AvgHR = ptr(round1(*a.AvgHR))
	}
	return w
}

// display renders a number the way the owner reads it, so the digest quotes this string and the
// check that no number is invented has a fixed set to compare against.
func display(unit string, v float64) string {
	switch unit {
	case "seconds":
		s := int(math.Round(v / 60)) // minutes
		if s >= 60 {
			return fmt.Sprintf("%dh %02dm", s/60, s%60)
		}
		return fmt.Sprintf("%dm", s)
	case "minutes":
		return fmt.Sprintf("%.0f min", v)
	case "bpm":
		return fmt.Sprintf("%.0f bpm", v)
	case "ms":
		return fmt.Sprintf("%.0f ms", v)
	case "kcal":
		return fmt.Sprintf("%.0f kcal", v)
	case "meters":
		return fmt.Sprintf("%.1f km", v/1000)
	case "steps":
		return thousands(int(math.Round(v)))
	}
	return fmt.Sprintf("%.0f", v)
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}

func meanSD(v []float64) (mu, sd float64) {
	for _, x := range v {
		mu += x
	}
	mu /= float64(len(v))
	if len(v) < 2 {
		return mu, 0
	}
	for _, x := range v {
		sd += (x - mu) * (x - mu)
	}
	return mu, math.Sqrt(sd / float64(len(v)-1))
}

func ptr[T any](v T) *T        { return &v }
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
