package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/satishthakur/chhaya/internal/memtest"
)

const memtestUsage = `chhayactl memtest check [-memory DIR]

  check  read Personal memory and its .tests directory (probes.json, golden.json) and report:
         a fact line with no proposal id, an id on two lines, a probe about a fact that is not
         in memory, a malformed probe or golden question (fail); a fact with no probe (warn).
         Runs no model. Exits non-zero on any fail, and on a test file it cannot read.

DIR defaults to $CHHAYA_GATEWAY_HOME/memory, or ~/.chhaya/gateway/memory.
`

func defaultMemoryDir() string {
	if h := os.Getenv("CHHAYA_GATEWAY_HOME"); h != "" {
		return filepath.Join(h, "memory")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".chhaya", "gateway", "memory")
}

func runMemtest(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprint(out, memtestUsage)
		return fmt.Errorf("usage: chhayactl memtest check")
	}
	fs := flag.NewFlagSet("memtest check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("memory", defaultMemoryDir(), "the memory root")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	r, err := memtest.Check(*dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%d fact(s), %d probe(s), %d golden question(s)\n", len(r.Facts), len(r.Probes), len(r.Golden))
	for _, p := range r.Problems {
		fmt.Fprintf(out, "  %-4s  %s\n", p.Level, p.Msg)
	}
	if n := r.Failures(); n > 0 {
		return fmt.Errorf("%d failure(s)", n)
	}
	return nil
}
