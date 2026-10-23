// chhayactl is the operator's side of the gateway. It is a separate binary so
// that verifying the audit log never runs inside the process that writes it,
// and so the kill switch has a path that does not need the daemon to be healthy.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
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
  policy explain   print the values the gateway is enforcing
`)
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
