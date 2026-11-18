package nightly

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/satishthakur/chhaya/internal/check"
)

// Status of one test row. Fail and Flag are what the headline counts; Pending, NA and NotRun are
// shown but are not passes.
type Status string

const (
	Pass    Status = "Pass"
	Fail    Status = "Fail"
	Flag    Status = "Flag"
	Pending Status = "Pending"
	NA      Status = "N/A"
	NotRun  Status = "Not run"
)

// Type says how a test decides. A deterministic test gives the same answer on the same input; a
// probabilistic one asks a model and can vary between runs.
type Type string

const (
	Deterministic Type = "Deterministic"
	Probabilistic Type = "Probabilistic"
)

// Row is one test. Detail is shown when the row is opened.
type Row struct {
	Name   string
	Type   Type
	Status Status
	Note   string // one line shown beside the name
	Detail string
}

// Section is one group of tests, shown collapsed with its totals.
type Section struct {
	Title string
	Blurb string
	Rows  []Row
}

func (s Section) Count(st Status) int {
	n := 0
	for _, r := range s.Rows {
		if r.Status == st {
			n++
		}
	}
	return n
}

// Totals is the one-line summary of a section.
func (s Section) Totals() string {
	var parts []string
	for _, st := range []Status{Fail, Flag, Pass, Pending, NA, NotRun} {
		if n := s.Count(st); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(st))))
		}
	}
	return strings.Join(parts, " · ")
}

// Worst is the section's headline status: fail beats not-run beats flag beats pass.
func (s Section) Worst() Status {
	for _, st := range []Status{Fail, NotRun, Flag} {
		if s.Count(st) > 0 {
			return st
		}
	}
	if s.Count(Pass) == 0 && len(s.Rows) > 0 {
		return Pending
	}
	return Pass
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Sections builds the report's tables from the input. Every test that exists is listed, passed or not.
func Sections(in Input) []Section {
	return []Section{in.traceSection(), in.memtestSection(), in.retrievalSection(), in.goldenSection(), in.judgeSection()}
}

func (in Input) traceSection() Section {
	sec := Section{Title: "Trace checks", Blurb: "Plain code over each exported conversation of the days " + strings.Join(in.Days, " and ") + ". Same traces, same answer."}
	for _, sm := range check.Summarize(in.Results, in.Checks) {
		r := Row{Name: sm.Check, Type: Deterministic, Status: Pass}
		for _, c := range in.Checks {
			if c.Name == sm.Check {
				r.Detail = c.Doc
			}
		}
		var lines []string
		for _, t := range in.Results {
			for _, f := range check.Sorted(t.Findings) {
				if f.Check != sm.Check || f.Level == check.Info {
					continue
				}
				at := ""
				if f.MessageID != 0 {
					at = fmt.Sprintf(" msg %d", f.MessageID)
				}
				lines = append(lines, fmt.Sprintf("%s (%s)%s [%s]: %s", t.ID, t.Source, at, f.Level, strings.Join(strings.Fields(f.Detail), " ")))
			}
		}
		switch {
		case sm.Ran == 0:
			r.Status, r.Note = NA, "applied to no trace"
		case len(lines) > 0 && sm.Level == check.Fail:
			r.Status, r.Note = Fail, fmt.Sprintf("%d finding(s) in %d trace(s)", sm.Findings, sm.Fired)
		case len(lines) > 0:
			r.Status, r.Note = Flag, fmt.Sprintf("%d finding(s) in %d trace(s)", sm.Findings, sm.Fired)
		default:
			r.Note = fmt.Sprintf("ran on %d trace(s)", sm.Ran)
		}
		if len(lines) > 0 {
			r.Detail += "\n\n" + strings.Join(lines, "\n")
		}
		sec.Rows = append(sec.Rows, r)
	}
	if len(in.Problems) > 0 {
		sec.Rows = append(sec.Rows, Row{Name: "Every trace could be read", Type: Deterministic, Status: Fail,
			Note: fmt.Sprintf("%d trace(s) not checked", len(in.Problems)), Detail: strings.Join(in.Problems, "\n")})
	}
	return sec
}

func (in Input) memtestSection() Section {
	sec := Section{Title: "Memory tests: structure", Blurb: "Plain code over the memory files and their .tests directory."}
	if in.Memtest == nil {
		sec.Rows = []Row{{Name: "Memory tests ran", Type: Deterministic, Status: NotRun, Note: "could not read the memory or its tests"}}
		return sec
	}
	var fails, warns []string
	for _, p := range in.Memtest.Problems {
		if p.Level == "fail" {
			fails = append(fails, p.Msg)
		} else {
			warns = append(warns, p.Msg)
		}
	}
	row := func(name, ok string, bad []string, level Status) Row {
		r := Row{Name: name, Type: Deterministic, Status: Pass, Note: ok}
		if len(bad) > 0 {
			r.Status, r.Note, r.Detail = level, fmt.Sprintf("%d problem(s)", len(bad)), strings.Join(bad, "\n")
		}
		return r
	}
	sec.Rows = []Row{
		row("Fact ids, probes and golden file hold together", fmt.Sprintf("%d facts, %d probes, %d golden", len(in.Memtest.Facts), len(in.Memtest.Probes), len(in.Memtest.Golden)), fails, Fail),
		row("Every fact has a probe", "no uncovered fact", warns, Flag),
	}
	return sec
}

func (in Input) canaryMissing(title string) (Section, bool) {
	switch {
	case in.Run == nil:
		return Section{Title: title, Rows: []Row{{Name: "Canary ran", Type: Deterministic, Status: NotRun, Note: "no result file", Detail: in.RunNote}}}, true
	case in.runStale():
		return Section{Title: title, Rows: []Row{{Name: "Canary ran last night", Type: Deterministic, Status: NotRun,
			Note: "last run " + in.Run.RanAt.UTC().Format("2006-01-02 15:04 UTC")}}}, true
	}
	return Section{}, false
}

func (in Input) retrievalSection() Section {
	title := "Memory canary: retrieval"
	if s, bad := in.canaryMissing(title); bad {
		return s
	}
	sec := Section{Title: title, Blurb: "A small model (" + in.Run.Model + ") answers each probe using only the memory tools. Pass = the fact's line reached the model. Whether the answer is right is the judge's, below."}
	for _, p := range in.Run.Result.Probes {
		r := Row{Name: p.ID, Type: Deterministic, Status: Pass, Note: clip(p.Question, 90),
			Detail: "Question: " + p.Question + "\nExpected: " + p.Expected + "\nAnswered: " + p.Answer}
		switch {
		case p.Err != "":
			r.Status, r.Note, r.Detail = Fail, "error: "+clip(p.Err, 80), p.Err
		case p.Fact == "":
			r.Status, r.Note = NA, "negative probe: no fact to retrieve"
		case !p.Retrieved:
			r.Status = Fail
			r.Note = "fact line never reached the model"
		}
		sec.Rows = append(sec.Rows, r)
	}
	return sec
}

func (in Input) goldenSection() Section {
	title := "Memory canary: golden questions"
	if s, bad := in.canaryMissing(title); bad {
		return s
	}
	sec := Section{Title: title, Blurb: "Real questions with known answers, owned by you. Pass = the answer has every required phrase and none of the forbidden ones."}
	for _, g := range in.Run.Result.Golden {
		r := Row{Name: g.ID, Type: Deterministic, Status: Pass, Note: clip(g.Question, 90), Detail: "Question: " + g.Question + "\nAnswered: " + g.Answer}
		switch {
		case g.Pass:
		case g.Pending:
			r.Status, r.Note = Pending, "known to fail until a fact exists: "+g.Why
		default:
			r.Status, r.Note = Fail, g.Why
		}
		sec.Rows = append(sec.Rows, r)
	}
	return sec
}

func (in Input) judgeSection() Section {
	title := "Memory canary: judge"
	if s, bad := in.canaryMissing(title); bad {
		for i := range s.Rows {
			s.Rows[i].Type = Probabilistic
		}
		return s
	}
	sec := Section{Title: title, Blurb: "A second model grades whether each answer matches the expected one. It can vary between runs and is not calibrated, so a Fail here is shown but does not change the overall result."}
	for _, p := range in.Run.Result.Probes {
		r := Row{Name: p.ID, Type: Probabilistic, Status: Pass, Detail: "Expected: " + p.Expected + "\nAnswered: " + p.Answer + "\nJudge: " + p.JudgeWhy}
		switch {
		case p.Judged == nil:
			r.Status, r.Note = NA, "not judged"
		case !*p.Judged:
			r.Status, r.Note = Fail, clip(p.JudgeWhy, 100)
		default:
			r.Note = clip(p.JudgeWhy, 100)
		}
		sec.Rows = append(sec.Rows, r)
	}
	return sec
}

// Gating reports whether a section's Fail rows change the overall result.
func gating(title string) bool { return !strings.HasSuffix(title, "judge") }

var page = template.Must(template.New("r").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Chhaya nightly {{.Day}}</title>
<style>
:root{--bg:#fff;--fg:#1c1c1e;--mut:#6b6b70;--line:#e2e2e6;--card:#f6f6f8;--fail:#b3261e;--flag:#8a5a00;--ok:#1a7f37}
:root:not([data-theme="light"]){color-scheme:light dark}
@media(prefers-color-scheme:dark){:root:not([data-theme="light"]){--bg:#111113;--fg:#ececef;--mut:#9a9aa2;--line:#2c2c30;--card:#1b1b1e;--fail:#ff8a80;--flag:#f0b95a;--ok:#5fd07f}}
:root[data-theme="dark"]{--bg:#111113;--fg:#ececef;--mut:#9a9aa2;--line:#2c2c30;--card:#1b1b1e;--fail:#ff8a80;--flag:#f0b95a;--ok:#5fd07f}
body{background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;margin:0 auto;padding:16px;max-width:900px}
.top{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
.top h1{font-size:1.15rem;margin:0 0 4px}.big{font-size:1.5rem;font-weight:700}
.mut{color:var(--mut)}.Fail,.NotRun{color:var(--fail);font-weight:600}.Flag{color:var(--flag);font-weight:600}.Pass{color:var(--ok);font-weight:600}.Pending,.NA{color:var(--mut);font-weight:600}
details.sec{border:1px solid var(--line);border-radius:10px;margin-top:12px}
details.sec>summary{padding:10px 14px;cursor:pointer;display:flex;gap:10px;align-items:baseline;flex-wrap:wrap}
details.sec>summary .t{font-weight:600}details.sec>summary .tot{color:var(--mut);margin-left:auto}
.blurb{margin:0 14px 8px;color:var(--mut);font-size:.9rem}
.head,details.row>summary{display:grid;grid-template-columns:minmax(0,3fr) 120px 80px;gap:8px;padding:6px 14px;border-top:1px solid var(--line);align-items:baseline}
.head{color:var(--mut);font-size:.8rem;text-transform:uppercase;letter-spacing:.04em}
details.row>summary{cursor:pointer;list-style:none}details.row>summary::-webkit-details-marker{display:none}
details.row>summary .n{overflow-wrap:anywhere}details.row>summary .note{display:block;color:var(--mut);font-size:.85rem}
details.row[open]>summary{background:var(--card)}
pre{white-space:pre-wrap;word-break:break-word;margin:0;padding:8px 14px 12px;font:13px/1.45 ui-monospace,monospace;background:var(--card)}
@media(max-width:560px){.head,details.row>summary{grid-template-columns:minmax(0,1fr) 70px}.head span:nth-child(2),details.row>summary .ty{display:none}}
</style></head><body>
<div class="top"><h1>Chhaya nightly {{.Day}}</h1>
<div class="big {{.Class}}">Overall: {{.Verdict}}</div>
<div>{{.Totals}}</div>
<div class="mut">Generated {{.Generated}} · days checked {{.Days}}. {{.Why}}</div>
<div class="mut">This page quotes your conversations; keep the link to yourself.</div></div>
{{range .Sections}}<details class="sec"><summary><span class="t">{{.Title}}</span> <span class="{{.Worst.Class}}">{{.Worst}}</span> <span class="tot">{{.Totals}}</span></summary>
<p class="blurb">{{.Blurb}}</p>
<div class="head"><span>Test</span><span>Type</span><span>Status</span></div>
{{range .Rows}}<details class="row"><summary><span class="n">{{.Name}}{{if .Note}}<span class="note">{{.Note}}</span>{{end}}</span><span class="ty mut">{{.Type}}</span><span class="{{.Status.Class}}">{{.Status}}</span></summary>{{if .Detail}}<pre>{{.Detail}}</pre>{{end}}</details>
{{end}}</details>
{{end}}</body></html>`))

// Class is a CSS class for the status (no spaces).
func (s Status) Class() string { return strings.ReplaceAll(string(s), " ", "") }

// HTML renders the report: the overall result, then every section collapsed, every test a row.
// html/template escapes every cell.
func HTML(in Input) string {
	secs := Sections(in)
	var pass, fail, flag, other int
	var ifail int
	for _, s := range secs {
		for _, r := range s.Rows {
			switch r.Status {
			case Pass:
				pass++
			case Fail:
				if gating(s.Title) {
					fail++
				} else {
					ifail++
				}
			case Flag:
				flag++
			case NotRun:
				fail++
			default:
				other++
			}
		}
	}
	v := in.Verdict()
	word := map[Verdict]string{OK: "PASS", Flagged: "PASS, with flags to read", Failed: "FAIL"}[v]
	why := ""
	if ifail > 0 {
		why = fmt.Sprintf("%d judge verdict(s) say Fail; the judge is probabilistic and does not count.", ifail)
	}
	d := map[string]any{
		"Day": in.day(), "Verdict": word, "Class": map[Verdict]string{OK: "Pass", Flagged: "Flag", Failed: "Fail"}[v],
		"Totals":    fmt.Sprintf("%d passed · %d failed · %d flagged · %d pending or not applicable", pass, fail, flag, other),
		"Generated": in.Now.UTC().Format("2006-01-02 15:04 UTC"), "Days": strings.Join(in.Days, ", "),
		"Why": why, "Sections": secs,
	}
	var b strings.Builder
	if err := page.Execute(&b, d); err != nil {
		return "report could not be rendered: " + template.HTMLEscapeString(err.Error())
	}
	return b.String()
}
