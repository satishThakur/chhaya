package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satishthakur/chhaya/internal/botguard"
	"github.com/satishthakur/chhaya/internal/dream"
	"github.com/satishthakur/chhaya/internal/memrun"
)

const dreamUsage = `chhayactl dream run [-s3 URI] [-day D | -trace ID] [-propose] [-budget 5] [-certs DIR] [-gateway URL]

  run    read conversations from the trace bucket, one at a time, with the memory agent: a model that
         looks in memory (memory.list, memory.get over mTLS, as the "dreamer") and proposes the facts
         you stated that memory lacks. Without -propose it is a dry run: it prints what it would send
         and sends nothing. With -propose each line reaches you as an approver card; nothing is written
         until you tap. Default: yesterday and today (IST), only Telegram conversations with a message
         you typed, none already read (the ledger, ~/.chhaya/dream-ledger.json). -trace ID reads one
         conversation, whatever the ledger says. -budget is the most proposals in one run.

chhayactl dream eval [-cases DIR] [-memory DIR] [-env-file FILE] [-model M] [-runs N]

  eval   run the extractor (a model that reads one conversation and the memory and suggests facts)
         through the filter, on every case, and check the outcome: the facts that must come back
         (and cite the owner's own message), silence where there is nothing to save, no repeat of a
         stored fact, a contradiction named and not added, a source that is not owner-typed refused,
         and the cap. Cases are JSON files: the shipped synthetic ones (internal/dream/cases) and,
         with -memory, your private ones in MEMORY/.tests/dream (real conversations; never in the
         public repo). Needs PROXY_API_KEY and CHHAYA_HERMES_API_BASE as memtest run does.
         Exits non-zero if any case fails. It proposes nothing and writes nothing.
`

func runDream(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "run" {
		return runDreamRun(args[1:], out, realDreamRunDeps())
	}
	if len(args) == 0 || args[0] != "eval" {
		fmt.Fprint(out, dreamUsage)
		return fmt.Errorf("usage: chhayactl dream eval")
	}
	fs := flag.NewFlagSet("dream eval", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cases := fs.String("cases", "internal/dream/cases", "directory of case files")
	mem := fs.String("memory", "", "also run the private cases in MEMORY/.tests/dream")
	envFile := fs.String("env-file", "deploy/.env.hermes", "file holding PROXY_API_KEY and CHHAYA_HERMES_API_BASE")
	model := fs.String("model", "claude-sonnet-5-5", "the extractor model")
	runs := fs.Int("runs", 1, "run each case this many times; the model is not deterministic")
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
	all, err := dream.LoadCases(*cases)
	if err != nil {
		return err
	}
	if *mem != "" {
		if _, err := os.Stat(filepath.Join(*mem, ".tests", "dream")); err == nil {
			priv, err := dream.LoadCases(filepath.Join(*mem, ".tests", "dream"))
			if err != nil {
				return err
			}
			all = append(all, priv...)
		}
	}
	if len(all) == 0 {
		return fmt.Errorf("no cases in %s", *cases)
	}
	m := newMemModel(base, key, *model)
	if h, ok := m.(*memrun.HTTPModel); ok {
		h.MaxTokens = 2048
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	var failed, in, outTok int
	fmt.Fprintf(out, "extractor %s, %d case(s)\n", *model, len(all))
	if *runs < 1 {
		return fmt.Errorf("-runs must be at least 1")
	}
	for _, c := range all {
		var r dream.CaseResult
		passed := 0
		for i := 0; i < *runs; i++ {
			x := dream.RunCase(ctx, m, c)
			in, outTok = in+x.Usage.In, outTok+x.Usage.Out
			if x.Pass {
				passed++
			} else if r.Why == nil || r.Pass {
				r = x // keep the first failing run to show
			}
			if i == 0 && x.Pass {
				r = x
			}
		}
		r.Pass = passed == *runs
		tag := "pass"
		if !r.Pass {
			tag, failed = "FAIL", failed+1
		}
		if *runs > 1 {
			tag += fmt.Sprintf(" %d/%d", passed, *runs)
		}
		fmt.Fprintf(out, "  %-8s  %s\n", tag, c.ID)
		for _, w := range r.Why {
			fmt.Fprintf(out, "          %s\n", w)
		}
		if !r.Pass {
			for _, x := range r.Result.Propose {
				fmt.Fprintf(out, "          proposed: %q (%s, msgs %v)\n", x.Entry, x.Path, x.Source)
			}
			for _, x := range r.Result.Contradictions {
				fmt.Fprintf(out, "          contradiction: %q reverses %s\n", x.Entry, x.Contradicts)
			}
		}
		for _, d := range r.Result.Dropped {
			fmt.Fprintf(out, "          (filter dropped %q: %s)\n", clipStr(d.Candidate.Entry, 60), d.Reason)
		}
	}
	fmt.Fprintf(out, "%d of %d passed; %d tokens in, %d out\n", len(all)-failed, len(all), in, outTok)
	if failed > 0 {
		return fmt.Errorf("%d case(s) failed", failed)
	}
	return nil
}
