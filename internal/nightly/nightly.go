// Package nightly builds the morning report: what the deterministic trace checks, the memory
// tests and the memory canary said overnight. It makes a short Telegram message (counts and check
// names, never message text) and an HTML page with the detail, kept in the private bucket and
// reached by a presigned link. docs/decisions.md, 2026-11-18.
package nightly

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/memtest"
)

// StaleAfter is how old a canary run may be and still count as last night's.
const StaleAfter = 26 * time.Hour

// Verdict is the headline.
type Verdict int

const (
	OK      Verdict = iota // nothing to look at
	Flagged                // read the report; nothing is broken
	Failed                 // an invariant broke, or something did not run
)

func (v Verdict) String() string { return [...]string{"ok", "flagged", "failed"}[v] }

// RunFile is what `memtest run -json` leaves for the report to read.
type RunFile struct {
	RanAt  time.Time     `json:"ran_at"`
	Model  string        `json:"model"`
	Result memrun.Result `json:"result"`
}

// WriteRunFile writes atomically, so the report never reads half a file.
func WriteRunFile(path string, r RunFile) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ReadRunFile(path string) (*RunFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r RunFile
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// Input is everything the report is made from. A nil Memtest or Run means that part did not run,
// and a part that did not run is not a pass.
type Input struct {
	Days     []string
	Results  []check.TraceResult
	Checks   []check.Check // the checks that produced Results; the report lists every one
	Problems []string      // traces that could not be read
	Memtest  *memtest.Report
	Run      *RunFile
	RunNote  string // why Run is nil, for the report
	Now      time.Time
}

func (in Input) runStale() bool { return in.Run == nil || in.Now.Sub(in.Run.RanAt) > StaleAfter }

func (in Input) counts() (fail int, flags map[string]int) {
	flags = map[string]int{}
	for _, r := range in.Results {
		for _, f := range r.Findings {
			switch f.Level {
			case check.Fail:
				fail++
			case check.Flag:
				flags[f.Check]++
			}
		}
	}
	return
}

func (in Input) Verdict() Verdict {
	fail, flags := in.counts()
	if fail > 0 || len(in.Problems) > 0 || in.Memtest == nil || in.Memtest.Failures() > 0 || in.runStale() || in.Run.Result.Gate() > 0 {
		return Failed
	}
	if len(flags) > 0 {
		return Flagged
	}
	return OK
}

func (in Input) day() string {
	if len(in.Days) > 0 {
		return in.Days[len(in.Days)-1]
	}
	return in.Now.Format("2006-01-02")
}

func (in Input) probeSummary() (got, scored, pass, fail, pend int) {
	if in.Run == nil {
		return
	}
	for _, p := range in.Run.Result.Probes {
		if p.Fact != "" {
			scored++
			if p.Retrieved {
				got++
			}
		}
	}
	for _, g := range in.Run.Result.Golden {
		switch {
		case g.Pending:
			pend++
		case g.Pass:
			pass++
		default:
			fail++
		}
	}
	return
}

func sortedFlags(m map[string]int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Message is the Telegram text. It holds counts and check names only: a finding's text quotes the
// owner's conversations and stays in the report.
func Message(in Input, link string) string {
	var b strings.Builder
	fail, flags := in.counts()
	head := map[Verdict]string{OK: "all clear", Flagged: "read the report", Failed: "FAILED"}[in.Verdict()]
	fmt.Fprintf(&b, "Chhaya nightly %s: %s\n", in.day(), head)
	nf := 0
	for _, n := range flags {
		nf += n
	}
	fmt.Fprintf(&b, "traces: %d checked, %d fail, %d flag", len(in.Results), fail, nf)
	if len(in.Problems) > 0 {
		fmt.Fprintf(&b, ", %d unreadable", len(in.Problems))
	}
	b.WriteString("\n")
	for _, k := range sortedFlags(flags) {
		fmt.Fprintf(&b, "  %s ×%d\n", k, flags[k])
	}
	switch {
	case in.Memtest == nil:
		b.WriteString("memory tests: DID NOT RUN\n")
	case in.Memtest.Failures() > 0:
		fmt.Fprintf(&b, "memory tests: %d failure(s)\n", in.Memtest.Failures())
	default:
		fmt.Fprintf(&b, "memory tests: ok (%d facts, %d probes, %d golden)\n", len(in.Memtest.Facts), len(in.Memtest.Probes), len(in.Memtest.Golden))
	}
	switch {
	case in.Run == nil:
		b.WriteString("memory canary: DID NOT RUN\n")
	case in.runStale():
		fmt.Fprintf(&b, "memory canary: STALE (last run %s)\n", in.Run.RanAt.UTC().Format("2006-01-02 15:04 UTC"))
	default:
		got, scored, pass, gfail, pend := in.probeSummary()
		fmt.Fprintf(&b, "memory canary: fact reached the model %d/%d, golden pass %d fail %d pending %d, judge %d wrong (informational)\n", got, scored, pass, gfail, pend, in.Run.Result.Wrong())
	}
	if link != "" {
		fmt.Fprintf(&b, "report: %s\n", link)
	}
	return b.String()
}

// Sender is the slice of the approver bot this needs.
type Sender interface {
	Send(ctx context.Context, text string) error
}

type PublishOptions struct {
	S3      string        // s3://bucket/evals; empty = do not upload
	Expires time.Duration // presigned link life
	Run     func(name string, args ...string) ([]byte, error)
	Send    Sender // nil = do not send
	Dir     string // where the HTML is written before upload
}

// Publish writes the report, uploads and presigns it, and sends the message. An upload or presign
// failure is returned, but the message still goes out and says the report is missing: a night's
// failures must not hide behind a broken link.
func Publish(ctx context.Context, in Input, o PublishOptions) (string, error) {
	var errs []string
	link := ""
	if o.S3 != "" {
		name := in.day() + ".html"
		local := filepath.Join(o.Dir, name)
		if err := os.WriteFile(local, []byte(HTML(in)), 0o600); err != nil {
			errs = append(errs, "write report: "+err.Error())
		} else {
			key := strings.TrimRight(o.S3, "/") + "/reports/" + name
			if _, err := o.Run("aws", "s3", "cp", "--only-show-errors", "--content-type", "text/html; charset=utf-8", local, key); err != nil {
				errs = append(errs, "upload report: "+err.Error())
			} else if out, err := o.Run("aws", "s3", "presign", key, "--expires-in", fmt.Sprint(int(o.Expires.Seconds()))); err != nil {
				errs = append(errs, "presign report: "+err.Error())
			} else {
				link = strings.TrimSpace(string(out))
			}
		}
	}
	msg := Message(in, link)
	if o.S3 != "" && link == "" {
		msg += "report not uploaded: see check.log on the box\n"
	} else if link != "" {
		msg += fmt.Sprintf("(link works for %s)\n", o.Expires.Round(time.Minute))
	}
	if o.Send != nil {
		if err := o.Send.Send(ctx, msg); err != nil {
			errs = append(errs, "send: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return msg, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return msg, nil
}
