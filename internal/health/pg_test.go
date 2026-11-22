package health

import (
	"context"
	"os"
	"testing"
	"time"
)

// These need a real TimescaleDB: set CHHAYA_TEST_PG_URL (make test-pg starts one). They are
// skipped, loudly, without it, because a hypertable and a continuous aggregate cannot be faked.
func testPG(t *testing.T) *PG {
	t.Helper()
	url := os.Getenv("CHHAYA_TEST_PG_URL")
	if url == "" {
		t.Skip("CHHAYA_TEST_PG_URL is not set: TimescaleDB tests skipped")
	}
	ctx := context.Background()
	p, err := OpenPG(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, tbl := range []string{"health_metrics", "health_activities", "health_sync_runs"} {
		if _, err := p.pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestPGMigrationsAreIdempotentAndMakeHypertables(t *testing.T) {
	p := testPG(t)
	ctx := context.Background()
	if err := p.migrate(ctx); err != nil {
		t.Fatalf("a second migration run must be a no-op: %v", err)
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name IN ('health_metrics','health_activities')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("hypertables: %d %v", n, err)
	}
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM timescaledb_information.continuous_aggregates WHERE view_name = 'health_metrics_weekly'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("continuous aggregate: %d %v", n, err)
	}
}

func TestPGUpsertReplacesADayAndRangeReadsItBack(t *testing.T) {
	p := testPG(t)
	ctx := context.Background()
	d := day(20)
	if err := p.Upsert(ctx, []Metric{{d, "steps", 1000}, {d, "hr_resting", 55}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Upsert(ctx, []Metric{{d, "steps", 9000}}, nil); err != nil { // the day grows through the day
		t.Fatal(err)
	}
	got, err := p.Range(ctx, day(19), day(21))
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, m := range got {
		if m.Name == "steps" && m.Value != 9000 {
			t.Fatalf("the later value must win: %+v", got)
		}
	}
	if none, _ := p.Range(ctx, day(21), day(25)); len(none) != 0 {
		t.Fatalf("range is half open: %+v", none)
	}
}

func TestPGUpsertIsAllOrNothing(t *testing.T) {
	p := testPG(t)
	ctx := context.Background()
	// A NUL byte in text is refused by Postgres, on the second row of the batch.
	err := p.Upsert(ctx, []Metric{{day(20), "steps", 1}, {day(20), "bad\x00name", 2}}, nil)
	if err == nil {
		t.Fatal("expected the batch to fail")
	}
	if got, _ := p.Range(ctx, day(19), day(22)); len(got) != 0 {
		t.Fatalf("the first row of a failed batch was kept: %+v", got)
	}
}

func TestPGActivitiesAndRuns(t *testing.T) {
	p := testPG(t)
	ctx := context.Background()
	hr := 148.0
	a := Activity{Time: day(20).Add(3 * time.Hour), Type: "running", DurationSeconds: 3000, AvgHR: &hr}
	if err := p.Upsert(ctx, nil, []Activity{a}); err != nil {
		t.Fatal(err)
	}
	if err := p.Upsert(ctx, nil, []Activity{a}); err != nil { // resent: still one row
		t.Fatal(err)
	}
	got, err := p.ActivitiesIn(ctx, day(20), day(21))
	if err != nil || len(got) != 1 || got[0].AvgHR == nil || *got[0].AvgHR != 148 || got[0].DistanceMeters != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if r, err := p.LastRun(ctx); err != nil || r != nil {
		t.Fatalf("no run yet: %+v %v", r, err)
	}
	if err := p.RecordRun(ctx, Run{StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-50 * time.Minute), Status: "ok", Days: []string{"2026-11-21"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordRun(ctx, Run{StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-9 * time.Minute), Status: "partial", Errors: []string{"stress: 500"}}); err != nil {
		t.Fatal(err)
	}
	r, err := p.LastRun(ctx)
	if err != nil || r == nil || r.Status != "partial" || len(r.Errors) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestPGWeeklyRollupHoldsRealAggregatesIncludingNewestDays(t *testing.T) {
	p := testPG(t)
	ctx := context.Background()
	var ms []Metric
	for i := 0; i < 14; i++ {
		ms = append(ms, Metric{Time: day(1 + i), Name: "steps", Value: float64(1000 * (i + 1))})
	}
	if err := p.Upsert(ctx, ms, nil); err != nil {
		t.Fatal(err)
	}
	// No refresh policy has run yet: the aggregate is not materialised, so a correct read proves the
	// real-time part of it works.
	got, err := p.Weekly(ctx, "steps", day(1).AddDate(0, 0, -14), day(1).AddDate(0, 0, 30))
	if err != nil || len(got) < 2 {
		t.Fatalf("%+v %v", got, err)
	}
	var days int
	var total float64
	for _, w := range got {
		days += w.Days
		total += w.Mean * float64(w.Days)
		if w.Min > w.Mean || w.Mean > w.Max || w.Days < 1 || w.Days > 7 {
			t.Fatalf("a week is inconsistent: %+v", w)
		}
	}
	if days != 14 || total != 105000 {
		t.Fatalf("the weeks must account for every day exactly once: days %d total %v", days, total)
	}
}
