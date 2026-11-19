package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/check"
	"github.com/satishthakur/chhaya/internal/memtest"
	"github.com/satishthakur/chhaya/internal/nightly"
	"github.com/satishthakur/chhaya/internal/trace"
)

const reportUsage = `chhayactl report nightly [-s3 URI] [-memory DIR] [-run-json FILE] [-env FILE] [-expires 6h] [-send]

  Runs the trace checks over yesterday and today (IST), the memory tests, and reads the memory
  canary's last result (memtest run -json). Prints the Telegram message. With -s3 it also uploads
  an HTML report to s3://bucket/evals/reports/DAY.html and puts a presigned link in the message;
  with -send it sends the message from the approver bot. The message holds counts and check names,
  never message text; the page holds the detail and sits in the private bucket. A part that did
  not run (no canary result, one older than 26 hours) shows as FAILED. Exits non-zero when the
  upload or the send failed, not when the night had findings.

  The link is signed with the box's role credentials, so it stops working when they rotate
  (about 6 hours); -expires cannot make it longer than that on the box.
`

type reportDeps struct {
	now    func() time.Time
	run    trace.Runner
	sender func(token string, chatID int64) nightly.Sender
}

func realReportDeps() reportDeps {
	td := realTraceDeps()
	return reportDeps{now: time.Now, run: td.run, sender: func(token string, chatID int64) nightly.Sender {
		return tgSender{tg: approval.NewHTTPTelegram(token, nil), chat: chatID}
	}}
}

func runReport(args []string, out io.Writer, d reportDeps) error {
	if len(args) == 0 || args[0] != "nightly" {
		fmt.Fprint(os.Stderr, reportUsage)
		return errors.New("report needs: nightly")
	}
	fs := flag.NewFlagSet("report nightly", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s3 := fs.String("s3", "", "s3://bucket/evals prefix (traces are read from it; the report is written to it)")
	dir := fs.String("dir", evalsDir(), "local evals directory when -s3 is not set")
	mem := fs.String("memory", defaultMemoryDir(), "the memory root")
	runJSON := fs.String("run-json", filepath.Join(defaultHome(), "backups", "memrun.json"), "the canary's last result")
	envFile := fs.String("env", "deploy/.env.gateway", "gateway env file (the approver bot's token and chat)")
	expires := fs.Duration("expires", 6*time.Hour, "how long the link should work")
	send := fs.Bool("send", false, "send the message")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *expires <= 0 || *expires > 7*24*time.Hour {
		return fmt.Errorf("-expires must be between 1s and 168h")
	}
	now := d.now()
	today := now.In(trace.IST)
	days := []string{today.AddDate(0, 0, -1).Format("2006-01-02"), today.Format("2006-01-02")}

	in := nightly.Input{Days: days, Now: now, Checks: check.All()}
	root := *dir
	if *s3 != "" {
		tmp, err := os.MkdirTemp("", "chhaya-report-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		root = tmp
	}
	st := &trace.Store{Dir: root, S3: *s3, Run: d.run, Now: d.now}
	checks := check.All()
	for _, day := range days {
		traces, problems, err := check.Load(st, day)
		if err != nil {
			in.Problems = append(in.Problems, day+": "+err.Error())
			continue
		}
		in.Problems = append(in.Problems, problems...)
		for _, t := range traces {
			in.Results = append(in.Results, check.Run(t, checks))
		}
	}
	if r, err := memtest.Check(*mem); err == nil {
		in.Memtest = &r
	}
	if rf, err := nightly.ReadRunFile(*runJSON); err == nil {
		in.Run = rf
	} else {
		in.RunNote = err.Error()
	}

	o := nightly.PublishOptions{S3: *s3, Expires: *expires, Run: d.run}
	if *s3 != "" {
		o.Dir = root
	}
	if *send {
		token, chat, err := approverTarget(*envFile)
		if err != nil {
			return err
		}
		o.Send = d.sender(token, chat)
	}
	msg, err := nightly.Publish(context.Background(), in, o)
	fmt.Fprint(out, msg)
	if err != nil {
		return err
	}
	if *send {
		fmt.Fprintln(out, "sent")
	}
	return nil
}

// approverTarget reads the approver bot's token and the one chat id it may write to.
func approverTarget(envFile string) (string, int64, error) {
	env, err := botguard.ReadEnvFile(envFile)
	if err != nil {
		return "", 0, fmt.Errorf("cannot read %s: %w", envFile, err)
	}
	chat, cerr := strconv.ParseInt(strings.TrimSpace(env["CHHAYA_APPROVER_ALLOWED_USERS"]), 10, 64)
	if env["CHHAYA_APPROVER_BOT_TOKEN"] == "" || cerr != nil {
		return "", 0, fmt.Errorf("%s needs CHHAYA_APPROVER_BOT_TOKEN and one numeric CHHAYA_APPROVER_ALLOWED_USERS", envFile)
	}
	return env["CHHAYA_APPROVER_BOT_TOKEN"], chat, nil
}
