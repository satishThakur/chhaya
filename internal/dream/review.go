package dream

import (
	"fmt"
	"github.com/satishthakur/chhaya/internal/jobreport"
	"html/template"
	"sort"
	"strings"
	"time"
)

type reviewItem struct {
	N                         int
	ID, Day, Path, Entry, Why string
	Reason, Contradicts       string
	Msgs, First               string
	Quotes                    []string
	Messages                  int
}

var reviewPage = template.Must(template.New("r").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Dreamer review {{.Generated}}</title>
<style>
:root{--bg:#fff;--fg:#1c1c1e;--mut:#6b6b70;--line:#e2e2e6;--card:#f6f6f8;--ok:#1a7f37;--bad:#b3261e;--flag:#8a5a00}
@media(prefers-color-scheme:dark){:root{--bg:#111113;--fg:#ececef;--mut:#9a9aa2;--line:#2c2c30;--card:#1b1b1e;--ok:#5fd07f;--bad:#ff8a80;--flag:#f0b95a}}
body{background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;margin:0 auto;padding:16px;max-width:860px}
.top{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
h1{font-size:1.15rem;margin:0 0 4px}h2{font-size:1.05rem;margin:1.6rem 0 .4rem;border-bottom:1px solid var(--line);padding-bottom:4px}
.mut{color:var(--mut)}.card{border:1px solid var(--line);border-radius:10px;padding:10px 14px;margin:10px 0}
.n{font-weight:700;color:var(--flag)}.entry{font-size:1.05rem;font-weight:600;margin:2px 0 6px}
blockquote{margin:6px 0;padding:6px 10px;border-left:3px solid var(--line);color:var(--mut);white-space:pre-wrap;word-break:break-word}
code{font-size:.85em}details summary{cursor:pointer}
</style></head><body>
<div class="top"><h1>Dreamer review</h1>
<div>{{.Summary}}</div>
<div class="mut">Generated {{.Generated}}. A dry run: nothing was sent to the approver and nothing was written. Tell me which numbers are good and which are bad. This page quotes your conversations; keep the link to yourself.</div></div>

<h2>Would propose ({{len .Props}})</h2>
{{range .Props}}<div class="card"><span class="n">#{{.N}}</span> <span class="mut">[{{.Path}}] · <code>{{.ID}}</code> {{.Day}} · msg {{.Msgs}}</span>
<div class="entry">{{.Entry}}</div>
{{range .Quotes}}<blockquote>{{.}}</blockquote>{{end}}
<div class="mut">{{.Why}}</div></div>
{{else}}<p class="mut">Nothing.</p>{{end}}

<h2>Contradictions ({{len .Contras}})</h2>
{{range .Contras}}<div class="card"><span class="mut">[{{.Path}}] · <code>{{.ID}}</code> {{.Day}} · msg {{.Msgs}}</span>
<div class="entry">{{.Entry}}</div><div class="mut">reverses stored fact <code>{{.Contradicts}}</code>; nothing would be added</div>
{{range .Quotes}}<blockquote>{{.}}</blockquote>{{end}}</div>
{{else}}<p class="mut">None.</p>{{end}}

<h2>Refused ({{len .Refs}})</h2>
<p class="mut">What the filter or the gateway turned down before it could reach you.</p>
{{range .Refs}}<div class="card"><code>{{.ID}}</code> {{.Day}}: {{.Entry}}<div class="mut">{{.Reason}}</div></div>
{{else}}<p class="mut">None.</p>{{end}}

<h2>Nothing to save ({{len .None}})</h2>
<p class="mut">If you remember stating a fact in one of these and it is missing from "Would propose", that is a miss: tell me which.</p>
<details><summary>{{len .None}} conversation(s) with no new fact</summary>
{{range .None}}<div class="mut"><code>{{.ID}}</code> {{.Day}} · {{.Messages}} messages{{if .First}} · “{{.First}}”{{end}}</div>{{end}}</details>

<h2>Errors ({{len .Errs}})</h2>
{{range .Errs}}<div class="card"><code>{{.ID}}</code> {{.Day}}: {{.Why}}</div>{{else}}<p class="mut">None.</p>{{end}}
</body></html>`))

// ReviewHTML renders a run for a human to read: what the agent would propose, numbered, each with
// the owner message it came from; contradictions; what was refused; the conversations with nothing
// to save; errors. Everything from a conversation or the model is escaped.
func ReviewHTML(s Summary, at time.Time) string {
	res := append([]TraceResult{}, s.Results...)
	sort.SliceStable(res, func(i, j int) bool { return res[i].Day < res[j].Day })
	var props, contras, refs, none, errs []reviewItem
	n := 0
	for _, r := range res {
		base := reviewItem{ID: r.ID, Day: r.Day, Messages: r.Messages, First: r.First}
		quoteFor := func(src []int64) []string {
			var q []string
			for _, id := range src {
				if t := r.Quotes[id]; t != "" {
					q = append(q, t)
				}
			}
			return q
		}
		if r.Err != "" {
			e := base
			e.Why = r.Err
			errs = append(errs, e)
		}
		for _, c := range r.Proposed {
			n++
			it := base
			it.N, it.Path, it.Entry, it.Why, it.Msgs, it.Quotes = n, c.Path, c.Entry, c.Rationale, joinIDs(c.Source), quoteFor(c.Source)
			props = append(props, it)
		}
		for _, c := range r.Contradictions {
			it := base
			it.Path, it.Entry, it.Contradicts, it.Msgs, it.Quotes = c.Path, c.Entry, c.Contradicts, joinIDs(c.Source), quoteFor(c.Source)
			contras = append(contras, it)
		}
		for _, d := range r.Refused {
			it := base
			it.Entry, it.Reason = d.Candidate.Entry, d.Reason
			refs = append(refs, it)
		}
		if r.Err == "" && len(r.Proposed)+len(r.Contradictions) == 0 {
			none = append(none, base)
		}
	}
	d := map[string]any{
		"Generated": at.UTC().Format("2006-01-02 15:04 UTC"),
		"Summary":   fmt.Sprintf("read %d, skipped %d (not an owner conversation, or nothing you typed), deferred %d, errors %d; %d tokens in, %d out", s.Read, s.Skipped, s.Deferred, s.Errors, s.Usage.In, s.Usage.Out),
		"Props":     props, "Contras": contras, "Refs": refs, "None": none, "Errs": errs,
	}
	var b strings.Builder
	if err := reviewPage.Execute(&b, d); err != nil {
		return "review could not be rendered: " + template.HTMLEscapeString(err.Error())
	}
	return b.String()
}

// SummaryMessage is the Telegram text for a run: a headline, counts and, when there is one, the
// link to the page. It never holds a line or a quote from a conversation. runErr is why the run
// could not finish (nil when it did).
func SummaryMessage(s Summary, propose bool, link, day string, runErr error) string {
	var b strings.Builder
	head := "nothing new"
	switch {
	case runErr != nil || s.Errors > 0:
		head = "FAILED"
	case s.Proposed > 0 && propose:
		head = fmt.Sprintf("%d proposed, tap the cards", s.Proposed)
	case s.Proposed > 0:
		head = fmt.Sprintf("dry run, %d would be proposed", s.Proposed)
	case !propose:
		head = "dry run, nothing new"
	}
	fmt.Fprintf(&b, "Chhaya dreamer %s: %s\n", day, head)
	fmt.Fprintf(&b, "read %d conversation(s), skipped %d, deferred %d; %d proposed, %d contradiction(s), %d refused, %d error(s)\n",
		s.Read, s.Skipped, s.Deferred, s.Proposed, s.Contradictions, s.Refused, s.Errors)
	if runErr != nil {
		fmt.Fprintf(&b, "stopped: %v\n", runErr)
	}
	if link != "" {
		fmt.Fprintf(&b, "report: %s\n", jobreport.Anchor(link, "open the page"))
	}
	return b.String()
}
