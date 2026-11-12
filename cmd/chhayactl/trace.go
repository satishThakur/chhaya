package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/trace"
)

const traceUsage = `chhayactl trace <command>

  export [-home DIR] [-out DIR] [-s3 URI] [-since 2d | -all] [-gap 60m]
         build one trace per conversation from Hermes' state.db and the audit log. A Hermes
         session lasts until /new, so a silence longer than -gap (default 60m, 0 = never) inside
         one starts a new trace: <id>, <id>.2, <id>.3. Write them to
         OUT/days/DAY/trace-ID.json and, with -s3 s3://bucket/evals, upload them. Read-only
         on state.db and the audit log; run it on the box after the backup.
  serve  [-dir DIR] [-s3 URI] [-port 8787]
         the annotation page, on 127.0.0.1 only. Reads traces from the bucket (or from DIR),
         saves annotations to DIR at once, and pushes them with the Export button.

  check  [-dir DIR] [-s3 URI] [-day YYYY-MM-DD] [-json] [-v]
         run the deterministic checks over exported traces (no model, no Hermes). With -s3 the
         bucket's copy is checked, not a local one. Exits non-zero on any "fail" finding or any
         trace that could not be read. -v also lists "info" lines.

DIR defaults to $CHHAYA_EVALS or ~/.chhaya/evals.
`

type traceDeps struct {
	now   func() time.Time
	run   trace.Runner
	query trace.Query
	// serve blocks until the server stops. Replaced in tests.
	serve func(ln net.Listener, h http.Handler) error
}

func realTraceDeps() traceDeps {
	return traceDeps{
		now:   time.Now,
		query: trace.SQLiteQuery,
		run: func(name string, args ...string) ([]byte, error) {
			out, err := exec.Command(name, args...).Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && len(ee.Stderr) > 0 {
					return nil, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
				}
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			return out, nil
		},
		serve: func(ln net.Listener, h http.Handler) error {
			return (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
		},
	}
}

func evalsDir() string {
	if d := os.Getenv("CHHAYA_EVALS"); d != "" {
		return d
	}
	return filepath.Join(defaultHome(), "evals")
}

// parseSince reads "2d", "36h" or any Go duration.
func parseSince(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("bad -since %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad -since %q", s)
	}
	return d, nil
}

func runTrace(args []string, out io.Writer, d traceDeps) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, traceUsage)
		return errors.New("trace needs a command")
	}
	fs := flag.NewFlagSet("trace "+args[0], flag.ContinueOnError)
	home := fs.String("home", defaultHome(), "the .chhaya directory (export)")
	dir := fs.String("dir", evalsDir(), "local evals directory")
	outDir := fs.String("out", "", "where export writes (default: the evals directory)")
	s3 := fs.String("s3", "", "s3://bucket/evals prefix")
	since := fs.String("since", "2d", "export sessions with a message newer than this")
	all := fs.Bool("all", false, "export every session")
	gap := fs.Duration("gap", trace.DefaultGap, "export: a silence longer than this inside a session starts a new trace (0 = never)")
	port := fs.Int("port", 8787, "port for serve (127.0.0.1 only)")
	day := fs.String("day", "", "check only this IST day (YYYY-MM-DD)")
	asJSON := fs.Bool("json", false, "check: print the report as JSON")
	verbose := fs.Bool("v", false, "check: also list info findings")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("trace %s takes no arguments", args[0])
	}

	switch args[0] {
	case "export":
		window, err := parseSince(*since)
		if err != nil {
			return err
		}
		if *all {
			window = 0
		}
		o := *outDir
		if o == "" {
			o = *dir
		}
		res, err := trace.Export(trace.ExportOptions{Home: *home, OutDir: o, S3: *s3, Since: window, Gap: *gap, Now: d.now(), Query: d.query, Run: d.run})
		fmt.Fprintln(out, res)
		if res.Flagged > 0 {
			fmt.Fprintf(out, "note: %d trace(s) have gateway calls no message claimed; the page shows them in red\n", res.Flagged)
		}
		return err
	case "serve":
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
		if err != nil {
			return err
		}
		defer ln.Close()
		st := &trace.Store{Dir: *dir, S3: *s3, Run: d.run, Now: d.now}
		fmt.Fprintf(out, "annotation page: http://%s/\n", ln.Addr())
		fmt.Fprintf(out, "traces from %s; annotations saved in %s\n", orLocal(*s3), *dir)
		return d.serve(ln, trace.NewHandler(st, trace.HostsFor(ln.Addr())))
	case "check":
		if *day != "" && !trace.ValidRef(*day, "x") {
			return fmt.Errorf("bad -day %q: want YYYY-MM-DD", *day)
		}
		dir := *dir
		if *s3 != "" {
			// The bucket is the system of record, and a resumed session is rewritten there. An
			// empty directory keeps a stale local copy from being read in its place.
			tmp, err := os.MkdirTemp("", "chhaya-check-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			dir = tmp
		}
		st := &trace.Store{Dir: dir, S3: *s3, Run: d.run, Now: d.now}
		traces, problems, err := check.Load(st, *day)
		if err != nil {
			return err
		}
		checks := check.All()
		results := make([]check.TraceResult, 0, len(traces))
		for _, t := range traces {
			results = append(results, check.Run(t, checks))
		}
		if *asJSON {
			if err := check.WriteJSON(out, results, checks, problems); err != nil {
				return err
			}
		} else {
			check.WriteText(out, results, checks, problems, *verbose)
		}
		if n := check.Failures(results); n > 0 || len(problems) > 0 {
			return fmt.Errorf("%d failure(s), %d trace(s) not checked", n, len(problems))
		}
		return nil
	default:
		fmt.Fprint(os.Stderr, traceUsage)
		return fmt.Errorf("unknown trace command %q", args[0])
	}
}

func orLocal(s3 string) string {
	if s3 == "" {
		return "the local directory"
	}
	return s3
}
