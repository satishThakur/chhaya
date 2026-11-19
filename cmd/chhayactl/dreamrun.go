package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/dream"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/trace"
)

// dreamRunDeps are what a test replaces: the trace store, the gateway connection and the clock.
type dreamRunDeps struct {
	now     func() time.Time
	run     trace.Runner
	connect func(ctx context.Context, endpoint, certDir string) (dreamGateway, error)
}

type dreamGateway interface {
	dream.Gateway
	Close() error
}

func realDreamRunDeps() dreamRunDeps {
	return dreamRunDeps{now: time.Now, run: realTraceDeps().run,
		connect: func(ctx context.Context, endpoint, certDir string) (dreamGateway, error) {
			return dream.Connect(ctx, endpoint, certDir)
		}}
}

func runDreamRun(args []string, out io.Writer, d dreamRunDeps) error {
	fs := flag.NewFlagSet("dream run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s3 := fs.String("s3", "", "s3://bucket/evals prefix to read conversations from")
	dir := fs.String("dir", evalsDir(), "local evals directory when -s3 is not set")
	day := fs.String("day", "", "read only this IST day (default: yesterday and today)")
	only := fs.String("trace", "", "read only this trace id, ignoring the ledger")
	propose := fs.Bool("propose", false, "send proposals to the approver (default: dry run)")
	budget := fs.Int("budget", 5, "most proposals in this run")
	certs := fs.String("certs", "deploy/certs", "directory with ca.crt, dreamer.crt, dreamer.key")
	gateway := fs.String("gateway", "https://127.0.0.1:8443/mcp", "the gateway's MCP endpoint")
	envFile := fs.String("env-file", "deploy/.env.hermes", "file holding PROXY_API_KEY and CHHAYA_HERMES_API_BASE")
	model := fs.String("model", "claude-sonnet-5-5", "the agent's model")
	ledgerPath := fs.String("ledger", filepath.Join(defaultHome(), "dream-ledger.json"), "which conversations were read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, base := os.Getenv("PROXY_API_KEY"), os.Getenv("CHHAYA_HERMES_API_BASE")
	if env, err := botguard.ReadEnvFile(*envFile); err == nil {
		if key == "" {
			key = env["PROXY_API_KEY"]
		}
		if base == "" {
			base = env["CHHAYA_HERMES_API_BASE"]
		}
	}
	if key == "" || base == "" {
		return fmt.Errorf("PROXY_API_KEY and CHHAYA_HERMES_API_BASE are needed, from the environment or %s", *envFile)
	}

	// Which days to read.
	today := d.now().In(trace.IST)
	days := []string{today.AddDate(0, 0, -1).Format("2006-01-02"), today.Format("2006-01-02")}
	if *day != "" {
		if !trace.ValidRef(*day, "x") {
			return fmt.Errorf("bad -day %q: want YYYY-MM-DD", *day)
		}
		days = []string{*day}
	}
	root := *dir
	if *s3 != "" {
		tmp, err := os.MkdirTemp("", "chhaya-dream-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		root = tmp
	}
	st := &trace.Store{Dir: root, S3: *s3, Run: d.run, Now: d.now}
	var traces []*trace.Trace
	if *only != "" {
		days = nil
		refs, err := st.Index()
		if err != nil {
			return err
		}
		for _, r := range refs {
			if r.ID == *only {
				days = append(days, r.Day)
			}
		}
		if len(days) == 0 {
			return fmt.Errorf("no trace %s in the store", *only)
		}
	}
	for _, dd := range days {
		ts, problems, err := check.Load(st, dd)
		if err != nil {
			return err
		}
		for _, p := range problems {
			fmt.Fprintf(out, "could not read %s\n", p)
		}
		for _, t := range ts {
			if *only == "" || t.ID == *only {
				traces = append(traces, t)
			}
		}
	}
	fmt.Fprintf(out, "%d conversation(s) in %v\n", len(traces), days)

	var led *dream.Ledger
	if *only == "" {
		var err error
		if led, err = dream.OpenLedger(*ledgerPath); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	gw, err := d.connect(ctx, *gateway, *certs)
	if err != nil {
		return err
	}
	defer gw.Close()
	m := newMemModel(base, key, *model)
	if h, ok := m.(*memrun.HTTPModel); ok {
		h.MaxTokens = 2048
	}
	sum, err := dream.RunTraces(ctx, m, gw, traces, dream.RunOptions{Budget: *budget, Propose: *propose, Ledger: led}, out)
	mode := "dry run: nothing was sent"
	if *propose {
		mode = "sent to the approver"
	}
	fmt.Fprintf(out, "read %d, skipped %d, deferred %d, errors %d; %d proposal(s) (%s), %d contradiction(s), %d refused; %d tokens in, %d out\n",
		sum.Read, sum.Skipped, sum.Deferred, sum.Errors, sum.Proposed, mode, sum.Contradictions, sum.Refused, sum.Usage.In, sum.Usage.Out)
	return err
}
