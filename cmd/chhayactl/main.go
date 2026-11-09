// chhayactl is the operator's side of the gateway. It is a separate binary so
// that verifying the audit log never runs inside the process that writes it,
// and so the kill switch has a path that does not need the daemon to be healthy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/satishthakur/chhaya/internal/approval"
	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/config"
	"github.com/satishthakur/chhaya/internal/state"
)

func main() {
	dataDir := flag.String("data", env("CHHAYA_DATA_DIR", "/var/lib/chhaya"), "gateway data directory")
	policyFile := flag.String("policy", env("CHHAYA_POLICY_FILE", "/etc/chhaya/policy.yaml"), "policy file")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "audit":
		if len(args) < 2 || args[1] != "verify" {
			usage()
			os.Exit(2)
		}
		err = runVerify(filepath.Join(*dataDir, "audit.jsonl"), os.Stdout)
	case "stop":
		err = runStop(filepath.Join(*dataDir, "state.json"), true, os.Stdout)
	case "resume":
		err = runStop(filepath.Join(*dataDir, "state.json"), false, os.Stdout)
	case "backup":
		err = runBackup(args[1:], os.Stdout, realBackupDeps())
	case "botcheck":
		fs := flag.NewFlagSet("botcheck", flag.ExitOnError)
		hermesEnv := fs.String("hermes-env", "deploy/.env.hermes", "Hermes env file")
		gatewayEnv := fs.String("gateway-env", "deploy/.env.gateway", "gateway env file")
		fs.Parse(args[1:])
		err = runBotCheck(*hermesEnv, *gatewayEnv, func(token string) botGetter {
			return approval.NewHTTPTelegram(token, nil)
		}, os.Stdout)
	case "policy":
		if len(args) < 2 || args[1] != "explain" {
			usage()
			os.Exit(2)
		}
		err = runExplain(*policyFile, os.Stdout)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "chhayactl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `chhayactl [-data DIR] [-policy FILE] <command>

  audit verify     walk the hash chain from genesis and report
  stop             set the kill switch without the daemon
  resume           clear the kill switch
  backup           create | verify | restore | push | pull | status (chhayactl backup for detail)
  botcheck         confirm both bot tokens belong to this CHHAYA_ENV (calls Telegram getMe)
  policy explain   print the values the gateway is enforcing
`)
}

// botGetter is the one Telegram call botcheck makes.
type botGetter = botguard.Telegram

// runBotCheck refuses unless both tokens are the bots CHHAYA_ENV says they should be.
// Tokens are read here and handed only to the Telegram client; nothing prints them.
func runBotCheck(hermesEnvPath, gatewayEnvPath string, mk func(token string) botGetter, out io.Writer) error {
	h, err := botguard.ReadEnvFile(hermesEnvPath)
	if err != nil {
		return fmt.Errorf("cannot read Hermes env file: %w", err)
	}
	g, err := botguard.ReadEnvFile(gatewayEnvPath)
	if err != nil {
		return fmt.Errorf("cannot read gateway env file: %w", err)
	}
	ht, at := h["TELEGRAM_BOT_TOKEN"], g["CHHAYA_APPROVER_BOT_TOKEN"]
	if ht == "" || at == "" {
		return errors.New("both TELEGRAM_BOT_TOKEN (Hermes) and CHHAYA_APPROVER_BOT_TOKEN (gateway) must be set")
	}
	if err := botguard.Verify(context.Background(), h["CHHAYA_ENV"], g["CHHAYA_ENV"], mk(ht), mk(at)); err != nil {
		return err
	}
	fmt.Fprintf(out, "ok: both bots belong to CHHAYA_ENV=%s\n", h["CHHAYA_ENV"])
	return nil
}

func runVerify(path string, out io.Writer) error {
	n, err := audit.Verify(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(out, "no audit log at %s — nothing to verify\n", path)
			return err
		}
		fmt.Fprintf(out, "FAILED after %d records\n", n)
		return err
	}
	fmt.Fprintf(out, "ok: %d records, chain intact from genesis\n", n)
	return nil
}

func runStop(statePath string, on bool, out io.Writer) error {
	st, err := state.Open(statePath)
	if err != nil {
		return err
	}
	if err := st.SetKillSwitch(on, time.Now()); err != nil {
		return err
	}
	if on {
		fmt.Fprintf(out, "kill switch ON — writes will be refused; reads still work\n")
	} else {
		fmt.Fprintf(out, "kill switch off\n")
	}
	return nil
}

func runExplain(policyFile string, out io.Writer) error {
	cfg, hash, err := config.Load(policyFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "policy hash: %s\n", hash)
	fmt.Fprintf(out, "rate limit:  %d calls per tool per hour\n", cfg.DefaultRatePerHour)
	fmt.Fprintf(out, "approvals:   %s before a card times out and denies\n", cfg.ApprovalTimeout)
	fmt.Fprintf(out, "branches:    %q\n", cfg.BranchPrefix)
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(out, "repos:       none — no repository is writable")
	} else {
		fmt.Fprintf(out, "repos:       %v\n", cfg.Repos)
	}
	keys := make([]string, 0, len(cfg.Caps))
	for key := range cfg.Caps {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c := cfg.Caps[key]
		fmt.Fprintf(out, "cap %-8s ₹%d per order, ₹%d per day\n", key+":", c.PerOrderMinor/100, c.PerDayMinor/100)
	}
	fmt.Fprintln(out, "\nTiers are decided in internal/policy, not here. This file supplies values only.")
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
