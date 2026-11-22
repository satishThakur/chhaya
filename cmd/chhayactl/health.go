package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/health"
)

const healthUsage = `chhayactl health pull -s3 s3://bucket/health [-url URL] [-env FILE] [-gateway-env FILE] [-send]

  Moves what the desktop's Garmin sync left in s3://bucket/health/inbox/ into the gateway: each day
  and run document is posted to the ingest listener (default http://127.0.0.1:8444, the box's
  loopback), which validates it with the same strict decoder as any other post, and is then filed
  under processed/ or rejected/. A document the gateway refuses as malformed is rejected/ and, with
  -send, reported on Telegram once. The gateway being down or the secret wrong leaves everything in
  the inbox for the next pass and exits non-zero. Nothing is printed when the inbox is empty.
  The ingest secret is read from -env (default deploy/.env.health).
`

func runHealth(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "pull" {
		fmt.Fprint(os.Stderr, healthUsage)
		return errors.New("health needs: pull")
	}
	fs := flag.NewFlagSet("health pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	s3 := fs.String("s3", "", "s3://bucket/health (inbox/, processed/ and rejected/ live under it)")
	url := fs.String("url", "http://127.0.0.1:8444", "the gateway's ingest listener")
	envFile := fs.String("env", "deploy/.env.health", "file holding CHHAYA_HEALTH_INGEST_SECRET")
	gwEnv := fs.String("gateway-env", "deploy/.env.gateway", "gateway env file (approver bot token, for -send)")
	send := fs.Bool("send", false, "report rejected documents on Telegram")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprint(os.Stderr, healthUsage)
		return err
	}
	if *s3 == "" {
		return errors.New("health pull needs -s3 s3://bucket/health")
	}
	env, err := botguard.ReadEnvFile(*envFile)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", *envFile, err)
	}
	secret := env["CHHAYA_HEALTH_INGEST_SECRET"]
	if secret == "" {
		return fmt.Errorf("%s has no CHHAYA_HEALTH_INGEST_SECRET", *envFile)
	}
	p := &health.Puller{S3: *s3, URL: *url, Secret: secret, Run: awsRun, HTTP: &http.Client{Timeout: 30 * time.Second}}
	res, perr := p.Pull(context.Background())
	if res.Ingested+len(res.Rejected)+res.Left > 0 {
		fmt.Fprintf(out, "%s health pull: ingested %d, rejected %d, left %d\n", time.Now().UTC().Format(time.RFC3339), res.Ingested, len(res.Rejected), res.Left)
	}
	for _, r := range res.Rejected {
		fmt.Fprintf(out, "  rejected: %s\n", r)
	}
	if *send && len(res.Rejected) > 0 {
		token, chat, err := approverTarget(*gwEnv)
		if err != nil {
			return errors.Join(perr, err)
		}
		msg := fmt.Sprintf("health sync: %d document(s) from the desktop were refused by the gateway and filed under rejected/ in the bucket:\n%s",
			len(res.Rejected), strings.Join(res.Rejected, "\n"))
		if _, err := approval.NewHTTPTelegram(token, nil).SendMessage(context.Background(), chat, msg, nil); err != nil {
			return errors.Join(perr, fmt.Errorf("telegram: %w", err))
		}
	}
	return perr
}

// awsRun runs the aws CLI and returns its stdout; stderr becomes part of the error.
func awsRun(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
