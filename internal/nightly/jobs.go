package nightly

import (
	"fmt"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/jobreport"
)

// Job is one unattended job and the stamp it left. A nil Stamp means it left none (never ran, or
// the file is unreadable): Why says what was found.
type Job struct {
	Name  string
	Doc   string // what the job does, shown in the report
	Stamp *jobreport.Stamp
	Why   string
}

// Status is Pass, or Fail with the reason: no stamp, an old stamp, or a run that finished broken.
func (j Job) Status(now time.Time) (Status, string) {
	switch {
	case j.Stamp == nil:
		return Fail, "no record of a run: " + j.Why
	case now.Sub(j.Stamp.Time) > StaleAfter:
		return Fail, fmt.Sprintf("last ran %s ago", age(now.Sub(j.Stamp.Time)))
	case j.Stamp.Failed != "":
		return Fail, "ran " + age(now.Sub(j.Stamp.Time)) + " ago but " + j.Stamp.Failed
	}
	return Pass, "ran " + age(now.Sub(j.Stamp.Time)) + " ago"
}

func age(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func (in Input) jobsFailing() int {
	n := 0
	for _, j := range in.Jobs {
		if st, _ := j.Status(in.Now); st != Pass {
			n++
		}
	}
	return n
}

func (in Input) jobsLine() string {
	var parts []string
	for _, j := range in.Jobs {
		if st, why := j.Status(in.Now); st == Pass {
			parts = append(parts, j.Name+" ok")
		} else {
			parts = append(parts, j.Name+" FAILED ("+why+")")
		}
	}
	return "jobs: " + strings.Join(parts, ", ")
}

func (in Input) jobsSection() Section {
	sec := Section{Title: "Nightly jobs", Blurb: "Each unattended job leaves a record when it finishes. A job with no record, a record older than 26 hours, or a run that ended broken is a failure: silence is how a broken job looks."}
	for _, j := range in.Jobs {
		st, note := j.Status(in.Now)
		r := Row{Name: j.Name, Type: Deterministic, Status: st, Note: note, Detail: j.Doc}
		if j.Stamp != nil {
			r.Detail += fmt.Sprintf("\n\nlast record: %s", j.Stamp.Time.UTC().Format("2006-01-02 15:04 UTC"))
			if j.Stamp.Detail != "" {
				r.Detail += "\n" + j.Stamp.Detail
			}
			if j.Stamp.Failed != "" {
				r.Detail += "\nfailed: " + j.Stamp.Failed
			}
		}
		sec.Rows = append(sec.Rows, r)
	}
	return sec
}
