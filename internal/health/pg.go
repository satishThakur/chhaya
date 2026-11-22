package health

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG is the TimescaleDB store. Metrics and activities are hypertables keyed by time; the weekly
// rollup is a continuous aggregate that includes the newest, not yet materialised, days.
type PG struct{ pool *pgxpool.Pool }

// migrations run in order, each statement on its own: CREATE MATERIALIZED VIEW ... WITH
// (timescaledb.continuous) cannot run inside a transaction. Every statement is idempotent, and
// a version is recorded only after all of its statements ran, so a crash part-way is retried.
var migrations = [][]string{{
	`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	`CREATE TABLE IF NOT EXISTS health_metrics (
		time timestamptz NOT NULL,
		metric text NOT NULL,
		value double precision NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (metric, time))`,
	`SELECT create_hypertable('health_metrics', 'time', chunk_time_interval => INTERVAL '90 days', if_not_exists => TRUE)`,
	`CREATE TABLE IF NOT EXISTS health_activities (
		time timestamptz NOT NULL,
		type text NOT NULL,
		duration_seconds double precision NOT NULL,
		distance_meters double precision,
		calories double precision,
		avg_hr double precision,
		max_hr double precision,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (type, time))`,
	`SELECT create_hypertable('health_activities', 'time', chunk_time_interval => INTERVAL '365 days', if_not_exists => TRUE)`,
	`CREATE TABLE IF NOT EXISTS health_sync_runs (
		id bigserial PRIMARY KEY,
		started_at timestamptz NOT NULL,
		finished_at timestamptz NOT NULL,
		status text NOT NULL,
		days text[] NOT NULL DEFAULT '{}',
		metrics int NOT NULL DEFAULT 0,
		activities int NOT NULL DEFAULT 0,
		errors text[] NOT NULL DEFAULT '{}')`,
	`CREATE MATERIALIZED VIEW IF NOT EXISTS health_metrics_weekly
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket(INTERVAL '7 days', time) AS week, metric,
		       avg(value) AS mean, min(value) AS min, max(value) AS max, count(*) AS days
		FROM health_metrics GROUP BY week, metric WITH NO DATA`,
	`SELECT add_continuous_aggregate_policy('health_metrics_weekly',
		start_offset => INTERVAL '120 days', end_offset => INTERVAL '1 hour',
		schedule_interval => INTERVAL '1 hour', if_not_exists => TRUE)`,
}}

// OpenPG connects and runs the migrations. It fails if either does: a gateway that cannot reach
// its health store should say so at boot, not on the first question.
func OpenPG(ctx context.Context, url string) (*PG, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("health store: %w", err)
	}
	p := &PG{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("health store: %w", err)
	}
	return p, nil
}

func (p *PG) Close() { p.pool.Close() }

func (p *PG) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// One migrator at a time, and the lock is held on this connection until it is released.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7263841)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7263841)`)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS health_migrations (version int PRIMARY KEY)`); err != nil {
		return err
	}
	for i, stmts := range migrations {
		v := i + 1
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM health_migrations WHERE version = $1)`, v).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		for _, s := range stmts {
			if _, err := conn.Exec(ctx, s); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := conn.Exec(ctx, `INSERT INTO health_migrations (version) VALUES ($1)`, v); err != nil {
			return err
		}
	}
	return nil
}

// Upsert writes everything in one transaction.
func (p *PG) Upsert(ctx context.Context, ms []Metric, acts []Activity) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	b := &pgx.Batch{}
	for _, m := range ms {
		b.Queue(`INSERT INTO health_metrics (time, metric, value) VALUES ($1, $2, $3)
			ON CONFLICT (metric, time) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, m.Time, m.Name, m.Value)
	}
	for _, a := range acts {
		b.Queue(`INSERT INTO health_activities (time, type, duration_seconds, distance_meters, calories, avg_hr, max_hr)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (type, time) DO UPDATE SET duration_seconds = EXCLUDED.duration_seconds,
			  distance_meters = EXCLUDED.distance_meters, calories = EXCLUDED.calories,
			  avg_hr = EXCLUDED.avg_hr, max_hr = EXCLUDED.max_hr, updated_at = now()`,
			a.Time, a.Type, a.DurationSeconds, a.DistanceMeters, a.Calories, a.AvgHR, a.MaxHR)
	}
	res := tx.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := res.Exec(); err != nil {
			res.Close()
			return err
		}
	}
	if err := res.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PG) RecordRun(ctx context.Context, r Run) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO health_sync_runs (started_at, finished_at, status, days, metrics, activities, errors)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, r.StartedAt, r.FinishedAt, r.Status, nz(r.Days), r.Metrics, r.Activities, nz(r.Errors))
	return err
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Range returns every metric row with from <= time < to.
func (p *PG) Range(ctx context.Context, from, to time.Time) ([]Metric, error) {
	rows, err := p.pool.Query(ctx, `SELECT time, metric, value FROM health_metrics WHERE time >= $1 AND time < $2 ORDER BY time, metric`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metric
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.Time, &m.Name, &m.Value); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActivitiesIn returns the workouts that started in [from, to).
func (p *PG) ActivitiesIn(ctx context.Context, from, to time.Time) ([]Activity, error) {
	rows, err := p.pool.Query(ctx, `SELECT time, type, duration_seconds, distance_meters, calories, avg_hr, max_hr
		FROM health_activities WHERE time >= $1 AND time < $2 ORDER BY time`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.Time, &a.Type, &a.DurationSeconds, &a.DistanceMeters, &a.Calories, &a.AvgHR, &a.MaxHR); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LastRun is the sidecar's newest run, or nil if there has been none.
func (p *PG) LastRun(ctx context.Context) (*Run, error) {
	var r Run
	err := p.pool.QueryRow(ctx, `SELECT started_at, finished_at, status, days, metrics, activities, errors
		FROM health_sync_runs ORDER BY finished_at DESC LIMIT 1`).Scan(&r.StartedAt, &r.FinishedAt, &r.Status, &r.Days, &r.Metrics, &r.Activities, &r.Errors)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Week is one 7-day bucket of one metric, from the continuous aggregate.
type Week struct {
	Start time.Time `json:"week_start"`
	Mean  float64   `json:"mean"`
	Min   float64   `json:"min"`
	Max   float64   `json:"max"`
	Days  int       `json:"days_with_data"`
}

// Weekly reads the rollup for one metric, oldest first.
func (p *PG) Weekly(ctx context.Context, metric string, from, to time.Time) ([]Week, error) {
	rows, err := p.pool.Query(ctx, `SELECT week, mean, min, max, days FROM health_metrics_weekly
		WHERE metric = $1 AND week >= $2 AND week < $3 ORDER BY week`, metric, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Week
	for rows.Next() {
		var w Week
		var n int64
		if err := rows.Scan(&w.Start, &w.Mean, &w.Min, &w.Max, &n); err != nil {
			return nil, err
		}
		w.Days = int(n)
		out = append(out, w)
	}
	return out, rows.Err()
}
