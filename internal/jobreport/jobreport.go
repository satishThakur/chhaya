// Package jobreport is how every scheduled job reports: one short Telegram summary (counts and
// names, never the text of a conversation) and, behind a presigned link in the summary, a detailed
// HTML page kept in the private bucket. The nightly eval report and the dreamer both use it, so a
// job that runs unattended is never just a line in a log file. docs/decisions.md, 2026-11-19.
package jobreport

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Sender is the slice of the approver bot a job needs.
type Sender interface {
	Send(ctx context.Context, text string) error
}

type Options struct {
	S3      string        // s3://bucket/evals; empty = do not upload
	Expires time.Duration // presigned link life
	Run     func(name string, args ...string) ([]byte, error)
	Send    Sender // nil = do not send
	Dir     string // where the page is written before upload
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Deliver writes html as NAME.html, uploads it to S3/reports/NAME.html, presigns it, builds the
// summary with message(link) and sends it. An upload or presign failure is returned, but the
// summary still goes out and says the report is missing: a night's failures must not hide behind a
// broken link. The returned text is what was (or would have been) sent.
func Deliver(ctx context.Context, o Options, name, html string, message func(link string) string) (string, error) {
	if !safeName.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("bad report name %q", name)
	}
	var errs []string
	link := ""
	if o.S3 != "" {
		local := filepath.Join(o.Dir, name+".html")
		if err := os.WriteFile(local, []byte(html), 0o600); err != nil {
			errs = append(errs, "write report: "+err.Error())
		} else {
			key := strings.TrimRight(o.S3, "/") + "/reports/" + name + ".html"
			if _, err := o.Run("aws", "s3", "cp", "--only-show-errors", "--content-type", "text/html; charset=utf-8", local, key); err != nil {
				errs = append(errs, "upload report: "+err.Error())
			} else if out, err := o.Run("aws", "s3", "presign", key, "--expires-in", fmt.Sprint(int(o.Expires.Seconds()))); err != nil {
				errs = append(errs, "presign report: "+err.Error())
			} else {
				link = strings.TrimSpace(string(out))
			}
		}
	}
	msg := message(link)
	if o.S3 != "" && link == "" {
		msg += "report not uploaded: see the job's log on the box\n"
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

// Anchor is a Telegram HTML link that shows text and hides the URL. A presigned URL is several
// hundred characters of signature, so it is never shown raw. The href is escaped (the URL carries
// & characters).
func Anchor(link, text string) string {
	return fmt.Sprintf("<a href=\"%s\">%s</a>", html.EscapeString(link), html.EscapeString(text))
}
