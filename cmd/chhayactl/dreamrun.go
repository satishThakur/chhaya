package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/dream"
	"github.com/satishthakur/chhaya/internal/jobreport"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/trace"
)

// dreamRunDeps are what a test replaces: the trace store, the gateway connection and the clock.
type dreamRunDeps struct {
	now     func() time.Time
	run     trace.Runner
	connect func(ctx context.Context, endpoint, certDir string) (dreamGateway, error)
	sender  func(token string, chatID int64) jobreport.Sender
}

type dreamGateway interface {
	dream.Gateway
	Close() error
}

func realDreamRunDeps() dreamRunDeps {
	return dreamRunDeps{now: time.Now, run: realTraceDeps().run,
		sender: func(token string, chatID int64) jobreport.Sender {
			return tgSender{tg: approval.NewHTTPTelegram(token, nil), chat: chatID}
		},
		connect: func(ctx context.Context, endpoint, certDir string) (dreamGateway, error) {
			return dream.Connect(ctx, endpoint, certDir)
		}}
}

func runDreamRun(args []string, out io.Writer, d dreamRunDeps) (retErr error) {
	fs := flag.NewFlagSet("dream run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s3 := fs.String("s3", "", "s3://bucket/evals prefix to read conversations from")
	dir := fs.String("dir", evalsDir(), "local evals directory when -s3 is not set")
	day := fs.String("day", "", "read only this IST day (default: yesterday and today)")
	all := fs.Bool("all", false, "read every day in the store")
	htmlOut := fs.String("html", "", "also write a review page (what it would propose, with the owner's words) to this file")
	publish := fs.Bool("publish", false, "with -s3: upload the review page to the bucket and print a presigned link")
	home := fs.String("home", defaultHome(), "the .chhaya directory (the run's record is written to its backups/)")
	send := fs.Bool("send", false, "with -s3: also send the summary and the link on Telegram (the approver bot)")
	gwEnv := fs.String("env", "deploy/.env.gateway", "gateway env file (the approver bot's token and chat)")
	expires := fs.Duration("expires", 6*time.Hour, "how long the link works")
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
	// A real run that dies before it finishes still leaves a record, marked broken: the morning
	// report must not read a night that failed early as a night with nothing to do.
	stamped := false
	if *propose {
		defer func() {
			if retErr != nil && !stamped {
				jobreport.WriteStamp(stampPath(*home, dreamStamp), jobreport.Stamp{Time: d.now(), Failed: "the run failed: " + retErr.Error()})
			}
		}()
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
	if *all {
		refs, err := st.Index()
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		days = nil
		for _, r := range refs {
			if !seen[r.Day] {
				seen[r.Day] = true
				days = append(days, r.Day)
			}
		}
		sort.Strings(days)
	}
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

	// Every job reports: a short summary, and a page behind a link. Built once the run is over, and
	// also when it could not start, so that a night that broke is not silence.
	var jo jobreport.Options
	if *send {
		token, chat, terr := approverTarget(*gwEnv)
		if terr != nil {
			return terr
		}
		jo.Send = d.sender(token, chat)
	}
	if *publish || *send {
		if *s3 == "" {
			return fmt.Errorf("-publish and -send need -s3")
		}
		jo.S3, jo.Expires, jo.Run = *s3, *expires, d.run
		tmp, terr := os.MkdirTemp("", "chhaya-dream-report-")
		if terr != nil {
			return terr
		}
		defer os.RemoveAll(tmp)
		jo.Dir = tmp
	} else if jo.Send != nil {
		return fmt.Errorf("-send needs -s3")
	}
	name := "dream-" + d.now().In(trace.IST).Format("2006-01-02")
	if !*propose {
		name += "-dry"
	}
	day0 := d.now().In(trace.IST).Format("2006-01-02")
	report := func(sum dream.Summary, runErr error) error {
		page := dream.ReviewHTML(sum, d.now())
		if *htmlOut != "" {
			if werr := os.WriteFile(*htmlOut, []byte(page), 0o600); werr != nil {
				return werr
			}
			fmt.Fprintf(out, "review page written to %s\n", *htmlOut)
		}
		if !*publish && !*send {
			return nil
		}
		msg, derr := jobreport.Deliver(ctx, jo, name, page, func(link string) string {
			return dream.SummaryMessage(sum, *propose, link, day0, runErr)
		})
		fmt.Fprint(out, "\n"+msg)
		return derr
	}

	gw, err := d.connect(ctx, *gateway, *certs)
	if err != nil {
		if rerr := report(dream.Summary{}, err); rerr != nil {
			fmt.Fprintf(out, "also could not report: %v\n", rerr)
		}
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
	// The morning report vouches for this job from this record. A dry run is a human looking, not
	// the job, so it leaves none; a run whose conversations errored is recorded as broken.
	if *propose {
		st := jobreport.Stamp{Time: d.now(), Detail: fmt.Sprintf("read %d, skipped %d, %d proposal(s), %d error(s)", sum.Read, sum.Skipped, sum.Proposed, sum.Errors)}
		switch {
		case err != nil:
			st.Failed = "the run failed: " + err.Error()
		case sum.Errors > 0:
			st.Failed = fmt.Sprintf("%d conversation(s) errored", sum.Errors)
		}
		stamped = true
		if serr := jobreport.WriteStamp(stampPath(*home, dreamStamp), st); serr != nil {
			fmt.Fprintf(out, "warning: could not record this run: %v\n", serr)
		}
	}
	if rerr := report(sum, err); rerr != nil {
		if err == nil {
			return rerr
		}
		fmt.Fprintf(out, "also could not report: %v\n", rerr)
	}
	return err
}
