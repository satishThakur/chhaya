package health

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"
)

// waitFor is how long the gateway waits for the database to come up at boot. Both start together
// from one compose file, and Postgres takes longer than the gateway does. Zero means one try.
func waitFor(s string) time.Duration {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 90 * time.Second
}

// openWithWait retries OpenPG every two seconds until it works or the wait is up, and returns the
// last error, so a database that never comes up is still a refusal to boot.
func openWithWait(ctx context.Context, url string, wait time.Duration) (*PG, error) {
	deadline := time.Now().Add(wait)
	for {
		pg, err := OpenPG(ctx, url)
		if err == nil {
			return pg, nil
		}
		if time.Now().Add(2*time.Second).After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(2 * time.Second):
		}
	}
}

// Runtime is a configured health module: what the tools read, and the listener the sidecar posts to.
type Runtime struct {
	Reader Reader
	Loc    *time.Location
	Ingest http.Handler
	Addr   string
	Close  func()
}

// FromEnv builds the module from the gateway's environment. With no CHHAYA_HEALTH_DB_URL it
// returns nil: the module is off and its tools do not exist. With a URL and no usable secret it
// is an error, never a listener without a door lock. dataDir is where the last-run record goes,
// for the morning report.
func FromEnv(ctx context.Context, getenv func(string) string, dataDir string, aud Auditor, killed func() bool) (*Runtime, error) {
	url := getenv("CHHAYA_HEALTH_DB_URL")
	if url == "" {
		return nil, nil
	}
	tz := getenv("CHHAYA_HEALTH_TZ")
	if tz == "" {
		tz = "Asia/Kolkata"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("CHHAYA_HEALTH_TZ %q: %w", tz, err)
	}
	if len(getenv("CHHAYA_HEALTH_INGEST_SECRET")) < minSecret {
		return nil, fmt.Errorf("CHHAYA_HEALTH_DB_URL is set, so CHHAYA_HEALTH_INGEST_SECRET must be at least %d characters", minSecret)
	}
	pg, err := openWithWait(ctx, url, waitFor(getenv("CHHAYA_HEALTH_DB_WAIT_SECONDS")))
	if err != nil {
		return nil, err
	}
	h, err := NewIngest(IngestOptions{Store: pg, Secret: getenv("CHHAYA_HEALTH_INGEST_SECRET"), Loc: loc, Audit: aud,
		Killed: killed, StampPath: filepath.Join(dataDir, "health-sync.json")})
	if err != nil {
		pg.Close()
		return nil, err
	}
	addr := getenv("CHHAYA_HEALTH_INGEST_LISTEN")
	if addr == "" {
		addr = "0.0.0.0:8444"
	}
	return &Runtime{Reader: pg, Loc: loc, Ingest: h, Addr: addr, Close: pg.Close}, nil
}
