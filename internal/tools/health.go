package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/health"
	"github.com/satishthakur/chhaya/internal/mcpsrv"
	"github.com/satishthakur/chhaya/internal/policy"
)

// healthTools are read-only. The store holds the owner's own Garmin data, written only by the
// sync sidecar through a secret Hermes does not hold, so its rows are Trusted (docs/health.md).
func healthTools(d Deps) []mcpsrv.Registration {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	loc := d.HealthLoc
	if loc == nil {
		loc = time.UTC
	}
	return []mcpsrv.Registration{
		{
			Name: "health.get_summary",
			Description: "Summarise the owner's Garmin health data over the last N days (1 to 90, default 7): for each metric, " +
				"the days with data, mean, min, max, the latest value, and for 6+ days the mean of the earlier and the later half; " +
				"workouts counted by type; how stale the data is; and how the last sync went. These are measured quantities, not advice. " +
				"Say what the numbers show and how many days they cover. This is fitness and recovery information, not medical advice. " +
				"If stale is true or there is no data, say so before anything else. Takes days.",
			Tier: policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(`{"type":"object","properties":{"days":{"type":"integer","minimum":1,"maximum":90,"description":"window in days, default 7"}}}`),
			Handle: func(ctx context.Context, args map[string]any) (any, error) {
				n, err := intArg(args, "days", 7, 1, 90)
				if err != nil {
					return nil, err
				}
				from, to := health.Window(n, loc, now())
				ms, err := d.Health.Range(ctx, from, to)
				if err != nil {
					return nil, fmt.Errorf("could not read health data: %w", err)
				}
				acts, err := d.Health.ActivitiesIn(ctx, from, to)
				if err != nil {
					return nil, fmt.Errorf("could not read workouts: %w", err)
				}
				run, err := d.Health.LastRun(ctx)
				if err != nil {
					return nil, fmt.Errorf("could not read the last sync: %w", err)
				}
				return health.Summarize(ms, acts, run, n, loc, now()), nil
			},
		},
		{
			Name: "health.get_digest",
			Description: "The owner's health for one day set against their own previous 30 days, already worked out: for sleep, HRV, resting heart rate, " +
				"stress and body battery, the value, the owner's usual (mean and spread), whether it is typical, notable or marked, above or below, " +
				"and whether that direction is better or worse; the 7-day trend; the notable findings in order; metrics with no value; workout load; " +
				"and how fresh the data is. Quote these figures, in their display form; never recompute or compare them yourself. " +
				"Nothing is judged without 14 days of history, and part-day figures are not judged on today. " +
				"date is YYYY-MM-DD, default today (or the newest day with data, which the note says). This is fitness and recovery information, not medical advice.",
			Tier: policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(`{"type":"object","properties":{"date":{"type":"string","description":"YYYY-MM-DD, default today"}}}`),
			Handle: func(ctx context.Context, args map[string]any) (any, error) {
				date := str(args["date"])
				at := now()
				today := at.In(loc)
				if date != "" {
					t, err := time.ParseInLocation("2006-01-02", date, loc)
					if err != nil {
						return nil, fmt.Errorf("date must be YYYY-MM-DD")
					}
					if t.After(today) || t.Before(today.AddDate(-1, 0, 0)) {
						return nil, fmt.Errorf("date must be within the last year and not in the future")
					}
				}
				anchor := today
				if date != "" {
					anchor, _ = time.ParseInLocation("2006-01-02", date, loc)
				}
				from := time.Date(anchor.Year(), anchor.Month(), anchor.Day()-health.DigestWindowBack, 0, 0, 0, 0, loc).UTC()
				to := time.Date(today.Year(), today.Month(), today.Day()+2, 0, 0, 0, 0, loc).UTC()
				ms, err := d.Health.Range(ctx, from, to)
				if err != nil {
					return nil, fmt.Errorf("could not read health data: %w", err)
				}
				acts, err := d.Health.ActivitiesIn(ctx, from, to)
				if err != nil {
					return nil, fmt.Errorf("could not read workouts: %w", err)
				}
				run, err := d.Health.LastRun(ctx)
				if err != nil {
					return nil, fmt.Errorf("could not read the last sync: %w", err)
				}
				return health.BuildDigest(ms, acts, run, date, loc, at), nil
			},
		},
		{
			Name: "health.get_series",
			Description: "One health metric day by day (or week by week) for the last N days, oldest first, so you can describe a pattern or find a specific day. " +
				"metric is one of: " + strings.Join(health.MetricNames(), ", ") + ". days is 1 to 365 (default 30). " +
				"bucket \"day\" (default) gives each day; \"week\" gives 7-day buckets with mean, min, max and the days with data in each. " +
				"Days with no data are absent, never zero.",
			Tier: policy.TierInternal, Kind: policy.Read, Trust: mcpsrv.Trusted,
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"metric":{"type":"string","description":"the metric name"},` +
				`"days":{"type":"integer","minimum":1,"maximum":365,"description":"window in days, default 30"},` +
				`"bucket":{"type":"string","enum":["day","week"],"description":"day (default) or week"}},` +
				`"required":["metric"]}`),
			Handle: func(ctx context.Context, args map[string]any) (any, error) {
				metric := str(args["metric"])
				if !health.Known(metric) {
					return nil, fmt.Errorf("unknown metric %q; use one of: %s", metric, strings.Join(health.MetricNames(), ", "))
				}
				n, err := intArg(args, "days", 30, 1, 365)
				if err != nil {
					return nil, err
				}
				bucket := str(args["bucket"])
				if bucket == "" {
					bucket = "day"
				}
				from, to := health.Window(n, loc, now())
				switch bucket {
				case "day":
					ms, err := d.Health.Range(ctx, from, to)
					if err != nil {
						return nil, fmt.Errorf("could not read health data: %w", err)
					}
					return map[string]any{"metric": metric, "bucket": "day", "days": health.SeriesOf(ms, metric, loc)}, nil
				case "week":
					ws, err := d.Health.Weekly(ctx, metric, from, to)
					if err != nil {
						return nil, fmt.Errorf("could not read the weekly rollup: %w", err)
					}
					return map[string]any{"metric": metric, "bucket": "week", "weeks": ws}, nil
				}
				return nil, fmt.Errorf("bucket must be day or week, not %q", bucket)
			},
		},
	}
}

// intArg reads an optional integer argument. JSON numbers arrive as float64; a fractional or
// out-of-range value is an error that says the range, so the agent corrects itself.
func intArg(args map[string]any, name string, def, lo, hi int) (int, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return def, nil
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", name, lo, hi)
	}
	n := int(f)
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s must be from %d to %d", name, lo, hi)
	}
	return n, nil
}
