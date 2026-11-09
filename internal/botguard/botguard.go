// Package botguard stops two machines polling the same Telegram bot.
//
// Two processes polling one bot token fight over getUpdates (Telegram answers
// 409), drop messages, and can deliver an approval tap to the wrong machine. There
// is one bot pair per environment, and each environment declares itself with
// CHHAYA_ENV=dev|prod in both env files. Before anything starts, getMe (read-only,
// consumes no updates) tells us who each token really is, and the names must match
// the declared environment. See docs/deployment.md.
//
// The convention: dev bot usernames contain "_dev_"; prod ones do not. The approver
// bot's username contains "approver"; Hermes' does not, which catches swapped tokens.
//
// Fail closed: an unreachable Telegram, a missing or unknown CHHAYA_ENV, or a name
// that fits neither rule is a refusal, never a warning.
package botguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Telegram is the one call we need. approval.Telegram satisfies it.
type Telegram interface {
	GetMe(ctx context.Context) (botID int64, username string, err error)
}

const (
	devMarker      = "_dev_"
	approverMarker = "approver"
)

// Verify returns nil only if both tokens belong to the bots that hermesEnv and
// gatewayEnv (the CHHAYA_ENV values from the two env files) say they should.
// Every problem found is reported, not just the first.
func Verify(ctx context.Context, hermesEnv, gatewayEnv string, hermes, approver Telegram) error {
	var problems []string

	env := ""
	switch {
	case !valid(hermesEnv) || !valid(gatewayEnv):
		if !valid(hermesEnv) {
			problems = append(problems, fmt.Sprintf("deploy/.env.hermes: CHHAYA_ENV is %s; it must be exactly dev or prod", show(hermesEnv)))
		}
		if !valid(gatewayEnv) {
			problems = append(problems, fmt.Sprintf("deploy/.env.gateway: CHHAYA_ENV is %s; it must be exactly dev or prod", show(gatewayEnv)))
		}
	case hermesEnv != gatewayEnv:
		problems = append(problems, fmt.Sprintf("CHHAYA_ENV disagrees: %q in .env.hermes, %q in .env.gateway", hermesEnv, gatewayEnv))
	default:
		env = hermesEnv
	}

	hid, hname, herr := whoami(ctx, hermes)
	aid, aname, aerr := whoami(ctx, approver)
	if herr != nil {
		problems = append(problems, fmt.Sprintf("Hermes bot token: cannot confirm which bot it is (%v)", herr))
	}
	if aerr != nil {
		problems = append(problems, fmt.Sprintf("approver bot token: cannot confirm which bot it is (%v)", aerr))
	}

	if herr == nil && aerr == nil && hid == aid {
		problems = append(problems, fmt.Sprintf("Hermes and the approver use the same bot (@%s); they must be two bots", hname))
	}
	if herr == nil {
		if strings.Contains(hname, approverMarker) {
			problems = append(problems, fmt.Sprintf("Hermes bot token is @%s, which looks like the approver bot; the tokens may be swapped", hname))
		}
		if env != "" {
			problems = append(problems, envMismatch("Hermes", hname, env)...)
		}
	}
	if aerr == nil {
		if !strings.Contains(aname, approverMarker) {
			problems = append(problems, fmt.Sprintf("approver bot token is @%s, which does not look like an approver bot; the tokens may be swapped", aname))
		}
		if env != "" {
			problems = append(problems, envMismatch("approver", aname, env)...)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("refusing to start:\n  - " + strings.Join(problems, "\n  - "))
}

func envMismatch(role, username, env string) []string {
	isDev := strings.Contains(username, devMarker)
	switch {
	case env == "dev" && !isDev:
		return []string{fmt.Sprintf("CHHAYA_ENV=dev but the %s bot is @%s, which is not a dev bot (dev usernames contain %q)", role, username, devMarker)}
	case env == "prod" && isDev:
		return []string{fmt.Sprintf("CHHAYA_ENV=prod but the %s bot is @%s, which is a dev bot", role, username)}
	}
	return nil
}

func valid(env string) bool { return env == "dev" || env == "prod" }

func show(s string) string {
	if s == "" {
		return "missing"
	}
	return fmt.Sprintf("%q", s)
}

// whoami returns the lowercased username (Telegram usernames are case-insensitive).
func whoami(ctx context.Context, t Telegram) (int64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, name, err := t.GetMe(ctx)
	if err != nil {
		return 0, "", err
	}
	if name == "" {
		return 0, "", errors.New("the bot reports no username, so it cannot be classified")
	}
	return id, strings.ToLower(name), nil
}

// ReadEnvFile parses KEY=VALUE lines. Blank lines and # comments are skipped, and
// one level of surrounding quotes is removed. It does not expand variables.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}
