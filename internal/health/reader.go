package health

import (
	"context"
	"time"
)

// Reader is what the health tools read. *PG satisfies it.
type Reader interface {
	Range(ctx context.Context, from, to time.Time) ([]Metric, error)
	ActivitiesIn(ctx context.Context, from, to time.Time) ([]Activity, error)
	LastRun(ctx context.Context) (*Run, error)
	Weekly(ctx context.Context, metric string, from, to time.Time) ([]Week, error)
}

// Window is [from, to) covering the last n local days including today.
func Window(n int, loc *time.Location, now time.Time) (from, to time.Time) {
	t := now.In(loc)
	today := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return today.AddDate(0, 0, -(n - 1)).UTC(), today.AddDate(0, 0, 2).UTC() // two days ahead: ingest accepts one for clock skew
}
