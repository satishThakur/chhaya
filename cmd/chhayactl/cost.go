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
	"github.com/satishthakur/chhaya/internal/cost"
	"github.com/satishthakur/chhaya/internal/trace"
)

const costUsage = `chhayactl cost report [flags]

  Prices every Hermes session from its token counts (config/pricing.yaml) and prints the
  table, dearest first. With -send it also sends one Telegram message per finished session
  that has not been reported (or grew since), from the approver bot, then remembers it.

  -home DIR       the .chhaya directory (default ~/.chhaya)
  -prices FILE    price table (default config/pricing.yaml)
  -env FILE       gateway env file holding the approver bot token (default deploy/.env.gateway)
  -send           send the messages (default: print only)
  -q              do not print the table (for cron)
  -idle DURATION  a session quiet this long counts as finished (default 35m)
  -since DURATION do not announce sessions quieter for longer than this (default 24h)
`

type costDeps struct {
	now   func() time.Time
	query trace.Query
	// sender builds the Telegram sender from the env file's token and chat id.
	sender func(token string, chatID int64) cost.Sender
}

func realCostDeps() costDeps {
	return costDeps{now: time.Now, query: trace.SQLiteQuery, sender: func(token string, chatID int64) cost.Sender {
		return tgSender{tg: approval.NewHTTPTelegram(token, nil), chat: chatID}
	}}
}

type tgSender struct {
	tg   approval.Telegram
	chat int64
}

func (t tgSender) Send(ctx context.Context, text string) error {
	_, err := t.tg.SendMessage(ctx, t.chat, text, nil)
	return err
}

func runCost(args []string, out io.Writer, d costDeps) error {
	if len(args) == 0 || args[0] != "report" {
		fmt.Fprint(os.Stderr, costUsage)
		return errors.New("cost needs: report")
	}
	fs := flag.NewFlagSet("cost report", flag.ContinueOnError)
	home := fs.String("home", defaultHome(), "the .chhaya directory")
	prices := fs.String("prices", "config/pricing.yaml", "price table")
	envFile := fs.String("env", "deploy/.env.gateway", "gateway env file")
	send := fs.Bool("send", false, "send the messages")
	quiet := fs.Bool("q", false, "do not print the table (for cron)")
	idle := fs.Duration("idle", 35*time.Minute, "quiet this long = finished")
	since := fs.Duration("since", 24*time.Hour, "do not announce sessions older than this")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pr, err := cost.LoadPrices(*prices)
	if err != nil {
		return err
	}
	raw, err := d.query(filepath.Join(*home, "hermes", "state.db"), cost.Query)
	if err != nil {
		return err
	}
	ss, err := cost.ParseSessions(raw)
	if err != nil {
		return err
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	statePath := cost.StatePath(filepath.Join(*home, "gateway"))
	st, err := cost.LoadState(statePath)
	if err != nil {
		return err
	}
	o := cost.Options{Prices: pr, State: st, Now: d.now(), Idle: *idle, Since: *since, Zone: ist}

	if !*quiet {
		cost.Table(out, ss, o)
	}
	due := cost.Due(ss, o)
	if !*send {
		fmt.Fprintf(out, "%d finished session(s) would be sent (use -send)\n", len(due))
		return nil
	}
	if len(due) == 0 {
		fmt.Fprintln(out, "nothing new to send")
		return nil
	}
	env, err := botguard.ReadEnvFile(*envFile)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", *envFile, err)
	}
	token := env["CHHAYA_APPROVER_BOT_TOKEN"]
	chat, cerr := strconv.ParseInt(strings.TrimSpace(env["CHHAYA_APPROVER_ALLOWED_USERS"]), 10, 64)
	if token == "" || cerr != nil {
		return fmt.Errorf("%s needs CHHAYA_APPROVER_BOT_TOKEN and one numeric CHHAYA_APPROVER_ALLOWED_USERS", *envFile)
	}
	sent, err := cost.Run(context.Background(), ss, o, d.sender(token, chat), statePath)
	fmt.Fprintf(out, "sent %d message(s)\n", len(sent))
	return err
}
