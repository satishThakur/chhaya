// Package memrun answers a memory's own tests with a weak model that has only the memory tools.
//
// For each probe and golden question it gives a small model two tools, memory_list and
// memory_get, the same code the gateway runs (memory.List, memory.Resolve), pointed at a temporary
// copy of the memory that holds no .tests, .proposals or .git. It records what the model did:
// whether the fact's line ever reached it, how many model calls and tool calls it took, and how
// many tokens. docs/memory-selftest.md S2.
//
// What gates and what does not. Whether the fact's line reached the model, and whether a golden
// answer contains and avoids the phrases it names, are checked by code: they gate. Whether a
// probe's answer means the expected one is a second model call; it is reported and does not gate,
// until the owner has compared it with their own reading (docs/observability-and-evals.md).
//
// It writes nothing: there is no write tool, and the memory it reads is a copy.
package memrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/memtest"
)

// Message is one chat message, in the shape both a scripted test model and the HTTP one use.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is one tool the model asked for; Args is the JSON the model wrote.
type ToolCall struct{ ID, Name, Args string }

// ToolDef describes a tool to the model.
type ToolDef struct {
	Name, Description string
	Schema            json.RawMessage
}

// Usage is the tokens one call used.
type Usage struct{ In, Out int }

// Model is one chat completion.
type Model interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolDef) (Message, Usage, error)
}

// Options. MaxSteps is the most model calls one question may take (default 8). With Judge set,
// JudgeModel grades the probe answers; it should not be the model being tested (nil falls back
// to it).
type Options struct {
	MaxSteps   int
	Judge      bool
	JudgeModel Model
}

// ProbeResult is what one probe did.
type ProbeResult struct {
	ID, Kind, Fact, Question, Expected, Answer, Err string
	Retrieved                                       bool   // the fact's line reached the model (not asked of a negative probe)
	Judged                                          *bool  // the judge's verdict; nil when not asked
	JudgeWhy                                        string // the judge's one-line reason
	Steps, ToolCalls, In, Out                       int
}

// GoldenResult is what one golden question did.
type GoldenResult struct {
	ID, Question, Answer, Why, Err string
	Pass, Pending                  bool
	Steps, ToolCalls, In, Out      int
}

// Result is a whole run.
type Result struct {
	Probes []ProbeResult
	Golden []GoldenResult
}

// Gate counts what makes the run fail: a probe about a fact whose line never reached the model, and
// a golden question that did not pass and is not marked pending. Judge verdicts are not in it.
func (r Result) Gate() int {
	n := 0
	for _, p := range r.Probes {
		if p.Fact != "" && !p.Retrieved {
			n++
		}
	}
	for _, g := range r.Golden {
		if !g.Pass && !g.Pending {
			n++
		}
	}
	return n
}

// Wrong counts probes the judge said did not match the expected answer.
func (r Result) Wrong() int {
	n := 0
	for _, p := range r.Probes {
		if p.Judged != nil && !*p.Judged {
			n++
		}
	}
	return n
}

// Tools are the two the model gets. Names use underscores because chat-completion tool names
// cannot hold a dot; they are memory.list and memory.get.
func Tools() []ToolDef {
	return []ToolDef{
		{Name: "memory_list", Description: "List every Personal memory document: its path (the argument memory_get takes), title and entry count. Takes no arguments. Returns paths only, not content.",
			Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "memory_get", Description: "Read one Personal memory document by path, e.g. \"taste/food\". Returns Markdown.",
			Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"document path, e.g. taste/food"}},"required":["path"]}`)},
	}
}

const system = `You are a personal assistant for one person, the owner. What you know about them is in a personal memory you read with the tools memory_list and memory_get. Do not guess: look in memory before you answer. If memory does not hold the answer, say so plainly. Answer in one or two sentences.`

// Run answers every probe and golden question in rep against a copy of the memory at root.
func Run(ctx context.Context, root string, rep memtest.Report, m Model, o Options) Result {
	if o.MaxSteps <= 0 {
		o.MaxSteps = 8
	}
	var res Result
	cp, err := copyMemory(root)
	if err != nil {
		fail := "could not copy the memory: " + err.Error()
		for _, p := range rep.Probes {
			res.Probes = append(res.Probes, ProbeResult{ID: p.ID, Kind: p.Kind, Fact: p.Fact, Question: p.Question, Err: fail})
		}
		for _, g := range rep.Golden {
			res.Golden = append(res.Golden, GoldenResult{ID: g.ID, Question: g.Question, Pending: g.Pending, Err: fail})
		}
		return res
	}
	defer os.RemoveAll(cp)

	jm := o.JudgeModel
	if jm == nil {
		jm = m
	}
	text := map[string]string{}
	for _, f := range rep.Facts {
		text[f.ID] = norm(f.Text)
	}
	// A model that cannot be reached fails every question the same way. Three failures in a row
	// end the run: the rest are marked skipped, not asked.
	failed := 0
	stopped := func() bool { return failed >= 3 }
	const skip = "stopped: three questions in a row could not reach the model"
	for _, p := range rep.Probes {
		if stopped() {
			res.Probes = append(res.Probes, ProbeResult{ID: p.ID, Kind: p.Kind, Fact: p.Fact, Question: p.Question, Err: skip})
			continue
		}
		a := ask(ctx, cp, m, p.Question, o.MaxSteps)
		if a.err != "" && a.answer == "" && a.steps == 1 {
			failed++
		} else {
			failed = 0
		}
		r := ProbeResult{ID: p.ID, Kind: p.Kind, Fact: p.Fact, Question: p.Question, Expected: p.Expected, Answer: a.answer, Err: a.err,
			Steps: a.steps, ToolCalls: a.calls, In: a.in, Out: a.out}
		if t, ok := text[p.Fact]; ok && t != "" {
			r.Retrieved = strings.Contains(norm(a.seen), t)
		}
		if o.Judge && a.err == "" {
			v, why, u, err := judge(ctx, jm, p, a.answer)
			r.In, r.Out = r.In+u.In, r.Out+u.Out
			if err == nil {
				r.Judged, r.JudgeWhy = &v, why
			}
		}
		res.Probes = append(res.Probes, r)
	}
	for _, g := range rep.Golden {
		if stopped() {
			res.Golden = append(res.Golden, GoldenResult{ID: g.ID, Question: g.Question, Pending: g.Pending, Err: skip, Why: skip})
			continue
		}
		a := ask(ctx, cp, m, g.Question, o.MaxSteps)
		if a.err != "" && a.answer == "" && a.steps == 1 {
			failed++
		} else {
			failed = 0
		}
		r := GoldenResult{ID: g.ID, Question: g.Question, Answer: a.answer, Err: a.err, Pending: g.Pending,
			Steps: a.steps, ToolCalls: a.calls, In: a.in, Out: a.out}
		r.Pass, r.Why = scoreGolden(g, a.answer, a.err)
		res.Golden = append(res.Golden, r)
	}
	return res
}

func scoreGolden(g memtest.Golden, answer, err string) (bool, string) {
	if err != "" {
		return false, err
	}
	low := strings.ToLower(answer)
	for _, w := range g.ExpectAll {
		if !strings.Contains(low, strings.ToLower(w)) {
			return false, fmt.Sprintf("the answer lacks %q", w)
		}
	}
	for _, w := range g.ExpectNone {
		if strings.Contains(low, strings.ToLower(w)) {
			return false, fmt.Sprintf("the answer contains %q", w)
		}
	}
	return true, ""
}

type asked struct {
	answer, err, seen string
	steps, calls      int
	in, out           int
}

// ask runs one question to an answer, executing the model's tool calls against the copy at cp.
func ask(ctx context.Context, cp string, m Model, question string, maxSteps int) asked {
	var a asked
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: question}}
	for a.steps < maxSteps {
		reply, u, err := m.Chat(ctx, msgs, Tools())
		a.steps++
		a.in, a.out = a.in+u.In, a.out+u.Out
		if err != nil {
			a.err = err.Error()
			return a
		}
		if len(reply.ToolCalls) == 0 {
			a.answer = strings.TrimSpace(reply.Content)
			return a
		}
		msgs = append(msgs, reply)
		for _, c := range reply.ToolCalls {
			a.calls++
			out := runTool(cp, c)
			a.seen += out + "\n"
			msgs = append(msgs, Message{Role: "tool", ToolCallID: c.ID, Content: out})
		}
	}
	a.err = fmt.Sprintf("no answer within %d steps", maxSteps)
	return a
}

// runTool is memory.list and memory.get, as the gateway runs them. A failure is the tool's result,
// as it would be for the real agent, so the model can recover from a wrong path.
func runTool(root string, c ToolCall) string {
	switch c.Name {
	case "memory_list":
		docs, truncated, err := memory.List(root)
		if err != nil {
			return "error: " + err.Error()
		}
		b, _ := json.Marshal(map[string]any{"documents": docs, "truncated": truncated})
		return string(b)
	case "memory_get":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(c.Args), &args); err != nil {
			return "error: arguments are not valid JSON"
		}
		p, err := memory.Resolve(root, args.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Sprintf("error: no memory document at %q", args.Path)
		}
		return string(b)
	}
	return "error: unknown tool " + c.Name
}

var verdictWord = regexp.MustCompile(`(?i)^(yes|no)[\s\-—–:.]*`)

func judge(ctx context.Context, m Model, p memtest.Probe, answer string) (bool, string, Usage, error) {
	prompt := fmt.Sprintf("Question: %s\nExpected answer: %s\nGiven answer: %s\n\nDoes the given answer say what the expected answer says? Wording may differ; extra detail is fine; contradiction or a missing key fact is not. When the expected answer lists alternatives joined by the word or, any one of them is enough. For an expected answer that says nothing is recorded, the given answer must also say nothing was found. Reply with YES or NO, then a dash and one short reason.",
		p.Question, p.Expected, answer)
	reply, u, err := m.Chat(ctx, []Message{{Role: "system", Content: "You grade answers strictly."}, {Role: "user", Content: prompt}}, nil)
	if err != nil {
		return false, "", u, err
	}
	text := strings.TrimSpace(reply.Content)
	return strings.HasPrefix(strings.ToUpper(text), "YES"), verdictWord.ReplaceAllString(text, ""), u, nil
}

// copyMemory copies the Markdown documents under root into a temporary directory. Dot-directories
// (.tests, .proposals, .git), symlinks and non-Markdown files are left out, so the model can read
// exactly what memory.get could and nothing the tests are made of.
func copyMemory(root string) (string, error) {
	dst, err := os.MkdirTemp("", "memrun-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }
