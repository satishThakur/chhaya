package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/memrun"
	"github.com/satishthakur/chhaya/internal/memtest"
	"github.com/satishthakur/chhaya/internal/nightly"
)

const memtestUsage = `chhayactl memtest <check | run> [-memory DIR]

  run    [-memory DIR] [-env-file FILE] [-json FILE] [-model M] [-judge=true] [-judge-model M] [-steps 8]
         answer every probe and golden question with a small model that has only memory_list and
         memory_get, against a copy of the memory (no .tests, .proposals or .git in it). Reports,
         per probe, whether the fact's line reached the model (MISS if not), how many model and
         tool calls it took and the tokens; per golden question, pass, fail or pending (known to
         fail until a fact exists). Exits non-zero on a MISS or a golden failure. -judge also asks
         the model whether each answer matches the expected one: WRONG lines are reported, and
         do not change the exit code until someone has compared them with their own reading.
         Needs PROXY_API_KEY and CHHAYA_HERMES_API_BASE, from the environment or -env-file
         (default deploy/.env.hermes). The key is never printed. -json FILE saves the
         result for the nightly report, even when the run fails. Costs a few cents.

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

// newMemModel builds the model the runner talks to; a test replaces it.
var newMemModel = func(base, key, model string) memrun.Model {
	return &memrun.HTTPModel{Base: base, Key: key, Model: model}
}

func runMemtest(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "run" {
		return runMemtestRun(args[1:], out)
	}
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprint(out, memtestUsage)
		return fmt.Errorf("usage: chhayactl memtest check | run")
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

func runMemtestRun(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("memtest run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("memory", defaultMemoryDir(), "the memory root")
	envFile := fs.String("env-file", "deploy/.env.hermes", "file holding PROXY_API_KEY and CHHAYA_HERMES_API_BASE")
	model := fs.String("model", "claude-haiku-5-5", "the weak model that answers")
	judgeModel := fs.String("judge-model", "claude-sonnet-5-5", "the model that grades answers (not the one tested)")
	judge := fs.Bool("judge", true, "ask the model whether each probe answer matches the expected one")
	steps := fs.Int("steps", 8, "most model calls per question")
	jsonOut := fs.String("json", "", "also write the result here, for `chhayactl report nightly` (written even when the run fails)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	key, base := os.Getenv("PROXY_API_KEY"), os.Getenv("CHHAYA_HERMES_API_BASE")
	if env, err := botguard.ReadEnvFile(*envFile); err == nil {
		if key == "" {
			key = env["PROXY_API_KEY"]
		}
		if base == "" {
			base = env["CHHAYA_HERMES_API_BASE"]
		}
	}
	if key == "" || base == "" {
		return fmt.Errorf("PROXY_API_KEY and CHHAYA_HERMES_API_BASE are needed, from the environment or %s", *envFile)
	}
	rep, err := memtest.Check(*dir)
	if err != nil {
		return err
	}
	if n := rep.Failures(); n > 0 {
		return fmt.Errorf("the tests do not hold together (%d failure(s)); run `chhayactl memtest check` first", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res := memrun.Run(ctx, *dir, rep, newMemModel(base, key, *model),
		memrun.Options{MaxSteps: *steps, Judge: *judge, JudgeModel: newMemModel(base, key, *judgeModel)})
	writeRun(out, res, *judge, *model+judgeNote(*judge, *judgeModel))
	if *jsonOut != "" {
		rf := nightly.RunFile{RanAt: time.Now().UTC(), Model: *model, Result: res}
		if err := nightly.WriteRunFile(*jsonOut, rf); err != nil {
			return fmt.Errorf("write %s: %w", *jsonOut, err)
		}
	}
	if n := res.Gate(); n > 0 {
		return fmt.Errorf("%d failure(s)", n)
	}
	return nil
}

func writeRun(out io.Writer, r memrun.Result, judged bool, model string) {
	var scored, got, in, outTok, calls, steps int
	for _, p := range r.Probes {
		if p.Fact != "" {
			scored++
			if p.Retrieved {
				got++
			}
		}
		in, outTok, calls, steps = in+p.In, outTok+p.Out, calls+p.ToolCalls, steps+p.Steps
	}
	fmt.Fprintf(out, "model %s\nprobes: %d, fact reached the model in %d of %d (negative probes are not scored)\n", model, len(r.Probes), got, scored)
	for _, p := range r.Probes {
		switch {
		case p.Err != "":
			fmt.Fprintf(out, "  ERR    %s  %s\n", p.ID, p.Err)
		case p.Fact != "" && !p.Retrieved:
			fmt.Fprintf(out, "  MISS   %s  %q (%d model calls, %d tool calls)\n         answered: %s\n", p.ID, clipStr(p.Question, 70), p.Steps, p.ToolCalls, clipStr(p.Answer, 100))
		case p.Judged != nil && !*p.Judged:
			fmt.Fprintf(out, "  WRONG  %s  %q\n         expected: %s\n         answered: %s\n         judge:    %s\n", p.ID, clipStr(p.Question, 70), clipStr(p.Expected, 200), clipStr(p.Answer, 400), clipStr(p.JudgeWhy, 200))
		}
	}
	if judged {
		fmt.Fprintf(out, "judge (uncalibrated, does not gate): %d answer(s) marked wrong\n", r.Wrong())
	}
	var pass, fail, pend int
	for _, g := range r.Golden {
		switch {
		case g.Pending:
			pend++
		case g.Pass:
			pass++
		default:
			fail++
		}
		in, outTok, calls, steps = in+g.In, outTok+g.Out, calls+g.ToolCalls, steps+g.Steps
	}
	fmt.Fprintf(out, "golden: %d (pass %d, fail %d, pending %d)\n", len(r.Golden), pass, fail, pend)
	for _, g := range r.Golden {
		tag := map[bool]string{true: "pass", false: "FAIL"}[g.Pass]
		if g.Pending {
			tag = "pend"
		}
		if !g.Pass {
			fmt.Fprintf(out, "  %-6s %s  %s\n         answered: %s\n", tag, g.ID, g.Why, clipStr(g.Answer, 100))
		}
	}
	n := len(r.Probes) + len(r.Golden)
	if n == 0 {
		n = 1
	}
	fmt.Fprintf(out, "%d model calls (%.1f per question), %d tool calls, %d tokens in, %d out\n", steps, float64(steps)/float64(n), calls, in, outTok)
}

func clipStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func judgeNote(on bool, m string) string {
	if !on {
		return ""
	}
	return " (judge " + m + ")"
}
