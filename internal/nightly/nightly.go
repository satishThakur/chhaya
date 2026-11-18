// Package nightly builds the morning report: what the deterministic trace checks, the memory
// tests and the memory canary said overnight. It makes a short Telegram message (counts and check
// names, never message text) and an HTML page with the detail, kept in the private bucket and
// reached by a presigned link. docs/decisions.md, 2026-11-18.
package nightly

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
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
	Problems []string // traces that could not be read
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

var page = template.Must(template.New("r").Funcs(template.FuncMap{"lvl": func(l check.Level) string { return string(l) }}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Chhaya nightly {{.Day}}</title>
<style>
:root{--bg:#fff;--fg:#1c1c1e;--mut:#6b6b70;--line:#e2e2e6;--fail:#b3261e;--flag:#8a5a00;--ok:#1a7f37}
@media(prefers-color-scheme:dark){:root{--bg:#111113;--fg:#ececef;--mut:#9a9aa2;--line:#2c2c30;--fail:#ff8a80;--flag:#f0b95a;--ok:#5fd07f}}
body{background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;margin:0 auto;padding:16px;max-width:860px}
h1{font-size:1.3rem}h2{font-size:1.05rem;margin-top:1.6rem;border-bottom:1px solid var(--line);padding-bottom:4px}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:4px 8px;border-bottom:1px solid var(--line);vertical-align:top}
.fail{color:var(--fail);font-weight:600}.flag{color:var(--flag);font-weight:600}.ok{color:var(--ok);font-weight:600}.mut{color:var(--mut)}
code{font-size:.9em}pre{white-space:pre-wrap;word-break:break-word;margin:0}
</style></head><body>
<h1>Chhaya nightly {{.Day}} <span class="{{.Class}}">{{.Verdict}}</span></h1>
<p class="mut">Generated {{.Generated}}. Days checked: {{.Days}}. This page quotes your conversations; keep the link to yourself.</p>

<h2>Trace checks</h2>
<p>{{.NTraces}} trace(s) checked, {{.NFail}} fail, {{.NFlag}} flag.</p>
{{if .Problems}}<p class="fail">{{len .Problems}} trace(s) could not be read:</p><ul>{{range .Problems}}<li><code>{{.}}</code></li>{{end}}</ul>{{end}}
{{range .Traces}}<h3><code>{{.ID}}</code> <span class="mut">{{.Day}} · {{.Source}}</span></h3>
<table>{{range .Findings}}<tr><td class="{{lvl .Level}}">{{.Level}}</td><td><code>{{.Check}}</code></td><td class="mut">{{if .MessageID}}msg {{.MessageID}}{{end}}</td><td><pre>{{.Detail}}</pre></td></tr>{{end}}</table>
{{else}}<p class="ok">No findings.</p>{{end}}

<h2>Memory tests</h2>
{{if .Memtest}}<p>{{len .Memtest.Facts}} facts, {{len .Memtest.Probes}} probes, {{len .Memtest.Golden}} golden questions.</p>
{{range .Memtest.Problems}}<p><span class="{{if eq .Level "fail"}}fail{{else}}flag{{end}}">{{.Level}}</span> {{.Msg}}</p>{{else}}<p class="ok">No problems.</p>{{end}}
{{else}}<p class="fail">Did not run.</p>{{end}}

<h2>Memory canary</h2>
{{if .Run}}<p class="mut">Ran {{.RanAt}} on {{.Model}}.{{if .Stale}} <span class="fail">Older than {{.StaleHours}} hours: not last night's.</span>{{end}}</p>
<p>Fact reached the model in {{.Got}} of {{.Scored}}. Golden: pass {{.GPass}}, fail {{.GFail}}, pending {{.GPend}}. Judge (informational): {{.Wrong}} marked wrong.</p>
{{range .Bad}}<table><tr><td class="{{.Tag}}">{{.Label}}</td><td><code>{{.ID}}</code></td><td><pre>{{.Text}}</pre></td></tr></table>{{end}}
<p class="mut">{{.Tokens}}</p>
{{else}}<p class="fail">Did not run. {{.RunNote}}</p>{{end}}
</body></html>`))

type badRow struct{ Tag, Label, ID, Text string }

// HTML renders the report. html/template escapes every finding.
func HTML(in Input) string {
	fail, flags := in.counts()
	nf := 0
	for _, n := range flags {
		nf += n
	}
	var shown []check.TraceResult
	for _, r := range in.Results {
		var fs []check.Finding
		for _, f := range check.Sorted(r.Findings) {
			if f.Level != check.Info {
				fs = append(fs, f)
			}
		}
		if len(fs) > 0 {
			r.Findings = fs
			shown = append(shown, r)
		}
	}
	v := in.Verdict()
	d := map[string]any{
		"Day": in.day(), "Verdict": v.String(), "Class": map[Verdict]string{OK: "ok", Flagged: "flag", Failed: "fail"}[v],
		"Generated": in.Now.UTC().Format("2006-01-02 15:04 UTC"), "Days": strings.Join(in.Days, ", "),
		"NTraces": len(in.Results), "NFail": fail, "NFlag": nf, "Problems": in.Problems, "Traces": shown,
		"Memtest": in.Memtest, "Run": in.Run != nil, "RunNote": in.RunNote,
	}
	if in.Run != nil {
		got, scored, pass, gfail, pend := in.probeSummary()
		var bad []badRow
		var inTok, outTok int
		for _, p := range in.Run.Result.Probes {
			inTok, outTok = inTok+p.In, outTok+p.Out
			switch {
			case p.Err != "":
				bad = append(bad, badRow{"fail", "ERR", p.ID, p.Err})
			case p.Fact != "" && !p.Retrieved:
				bad = append(bad, badRow{"fail", "MISS", p.ID, p.Question + "\nanswered: " + p.Answer})
			case p.Judged != nil && !*p.Judged:
				bad = append(bad, badRow{"flag", "WRONG", p.ID, p.Question + "\nexpected: " + p.Expected + "\nanswered: " + p.Answer + "\njudge: " + p.JudgeWhy})
			}
		}
		for _, g := range in.Run.Result.Golden {
			inTok, outTok = inTok+g.In, outTok+g.Out
			if !g.Pass && !g.Pending {
				bad = append(bad, badRow{"fail", "FAIL", g.ID, g.Question + "\n" + g.Why + "\nanswered: " + g.Answer})
			}
		}
		d["RanAt"] = in.Run.RanAt.UTC().Format("2006-01-02 15:04 UTC")
		d["Model"], d["Stale"], d["StaleHours"] = in.Run.Model, in.runStale(), int(StaleAfter.Hours())
		d["Got"], d["Scored"], d["GPass"], d["GFail"], d["GPend"] = got, scored, pass, gfail, pend
		d["Wrong"], d["Bad"], d["Tokens"] = in.Run.Result.Wrong(), bad, fmt.Sprintf("%d tokens in, %d out", inTok, outTok)
	}
	var b strings.Builder
	if err := page.Execute(&b, d); err != nil {
		return "report could not be rendered: " + template.HTMLEscapeString(err.Error())
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
