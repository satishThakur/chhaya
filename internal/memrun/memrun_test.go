package memrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishthakur/chhaya/internal/memtest"
)

const idA = "20261106-3f609264ada59fd2"
const idB = "20261109-d93c1711ba954bc5"

func memRoot(t *testing.T) string {
	root := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("interests.md", "# Interests\n\n- Loves reading books. <!-- proposal "+idA+", approved 2026-11-06 -->\n")
	put("family.md", "# Family\n\n- Son loves Real Madrid. <!-- proposal "+idB+", approved 2026-11-09 -->\n")
	put(".tests/probes.json", `[{"id":"SECRET-ANSWER"}]`)
	put(".proposals/x.json", `{"entry":"SECRET-PROPOSAL"}`)
	return root
}

// script is a model that replies from a list, so a test says exactly what the "weak model" does.
type script struct {
	replies []Message
	seen    [][]Message
	i       int
}

func (s *script) Chat(_ context.Context, msgs []Message, _ []ToolDef) (Message, Usage, error) {
	s.seen = append(s.seen, append([]Message(nil), msgs...))
	r := s.replies[min(s.i, len(s.replies)-1)]
	s.i++
	return r, Usage{In: 100, Out: 10}, nil
}

func call(id, name, args string) Message {
	return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: name, Args: args}}}
}
func say(s string) Message { return Message{Role: "assistant", Content: s} }

func probe(fact, kind string) memtest.Probe {
	return memtest.Probe{ID: "p1", Fact: fact, Kind: kind, Question: "What does the owner love?", Expected: "books"}
}

func TestAProbeIsRetrievedWhenItsFactReachesTheModel(t *testing.T) {
	m := &script{replies: []Message{call("c1", "memory_list", `{}`), call("c2", "memory_get", `{"path":"interests"}`), say("Books.")}}
	r := Run(context.Background(), memRoot(t), memtest.Report{
		Facts:  []memtest.Fact{{ID: idA, Path: "interests", Text: "Loves reading books."}},
		Probes: []memtest.Probe{probe(idA, "direct")},
	}, m, Options{})
	got := r.Probes[0]
	if !got.Retrieved || got.Steps != 3 || got.ToolCalls != 2 || got.Answer != "Books." || got.In != 300 {
		t.Fatalf("%+v", got)
	}
	if r.Gate() != 0 {
		t.Fatalf("gate %d", r.Gate())
	}
}

func TestAProbeAnsweredWithoutReadingMemoryIsNotRetrieved(t *testing.T) {
	m := &script{replies: []Message{say("Probably books.")}}
	r := Run(context.Background(), memRoot(t), memtest.Report{
		Facts:  []memtest.Fact{{ID: idA, Path: "interests", Text: "Loves reading books."}},
		Probes: []memtest.Probe{probe(idA, "direct")},
	}, m, Options{})
	if r.Probes[0].Retrieved || r.Gate() != 1 {
		t.Fatalf("a guess is not a retrieval: %+v gate=%d", r.Probes[0], r.Gate())
	}
}

func TestTheModelCannotSeeTheTestsOrStagedProposals(t *testing.T) {
	m := &script{replies: []Message{
		call("c1", "memory_get", `{"path":".tests/probes"}`),
		call("c2", "memory_get", `{"path":".proposals/x"}`),
		call("c3", "memory_list", `{}`),
		say("done"),
	}}
	Run(context.Background(), memRoot(t), memtest.Report{Probes: []memtest.Probe{{ID: "n", Kind: "negative", Question: "q", Expected: "e"}}}, m, Options{})
	all := ""
	for _, msgs := range m.seen {
		for _, x := range msgs {
			all += x.Content + "\n"
		}
	}
	if strings.Contains(all, "SECRET") {
		t.Fatalf("the model was shown test data or a staged proposal:\n%s", all)
	}
}

func TestRunWorksOnACopyAndLeavesTheLiveMemoryAlone(t *testing.T) {
	root := memRoot(t)
	before, _ := os.ReadFile(filepath.Join(root, "interests.md"))
	m := &script{replies: []Message{call("c1", "memory_get", `{"path":"interests"}`), say("x")}}
	Run(context.Background(), root, memtest.Report{Probes: []memtest.Probe{{ID: "n", Kind: "negative", Question: "q", Expected: "e"}}}, m, Options{})
	after, _ := os.ReadFile(filepath.Join(root, "interests.md"))
	if string(before) != string(after) {
		t.Fatal("the live memory changed")
	}
}

func TestALoopingModelIsStoppedAtMaxSteps(t *testing.T) {
	m := &script{replies: []Message{call("c", "memory_list", `{}`)}}
	r := Run(context.Background(), memRoot(t), memtest.Report{Probes: []memtest.Probe{probe(idA, "direct")}}, m, Options{MaxSteps: 4})
	if got := r.Probes[0]; got.Steps != 4 || got.Answer != "" || got.Err == "" {
		t.Fatalf("%+v", got)
	}
}

func TestGoldenQuestionsAreScoredOnTheAnswerText(t *testing.T) {
	g := []memtest.Golden{
		{ID: "yes", Question: "q", ExpectAll: []string{"heard"}},
		{ID: "no-repeat", Question: "q", ExpectNone: []string{"Ashtavakra"}},
		{ID: "pending", Question: "q", ExpectAll: []string{"zzz"}, Pending: true},
	}
	m := &script{replies: []Message{say("Yes, HEARD it."), say("Try Ashtavakra."), say("nope")}}
	r := Run(context.Background(), memRoot(t), memtest.Report{Golden: g}, m, Options{})
	pass := map[string]bool{}
	for _, x := range r.Golden {
		pass[x.ID] = x.Pass
	}
	if !pass["yes"] || pass["no-repeat"] || pass["pending"] {
		t.Fatalf("%v", pass)
	}
	if r.Gate() != 1 { // only no-repeat counts; the pending one is reported, not counted
		t.Fatalf("gate %d, want 1", r.Gate())
	}
}

func TestTheJudgeIsReportedButDoesNotGate(t *testing.T) {
	m := &script{replies: []Message{call("c1", "memory_get", `{"path":"interests"}`), say("Cricket."), say("NO")}}
	r := Run(context.Background(), memRoot(t), memtest.Report{
		Facts:  []memtest.Fact{{ID: idA, Path: "interests", Text: "Loves reading books."}},
		Probes: []memtest.Probe{probe(idA, "direct")},
	}, m, Options{Judge: true})
	got := r.Probes[0]
	if got.Judged == nil || *got.Judged || !got.Retrieved || r.Gate() != 0 {
		t.Fatalf("%+v gate=%d", got, r.Gate())
	}
	if r.Wrong() != 1 {
		t.Fatalf("wrong %d", r.Wrong())
	}
}

func TestHTTPModelSpeaksChatCompletions(t *testing.T) {
	var gotAuth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"memory_get","arguments":"{\"path\":\"interests\"}"}}]}}],"usage":{"prompt_tokens":120,"completion_tokens":9}}`)
	}))
	defer srv.Close()
	h := &HTTPModel{Base: srv.URL, Key: "sk-test", Model: "claude-haiku-5-5"}
	msg, u, err := h.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, Tools())
	if err != nil {
		t.Fatal(err)
	}
	if _, has := body["temperature"]; has {
		t.Fatal("temperature must not be sent: the API rejects it for current models")
	}
	if gotAuth != "Bearer sk-test" || body["model"] != "claude-haiku-5-5" || body["tools"] == nil {
		t.Fatalf("request: auth=%q body=%v", gotAuth, body)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "memory_get" || u.In != 120 || u.Out != 9 {
		t.Fatalf("%+v %+v", msg, u)
	}
}

func TestHTTPErrorsNeverEchoTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key sk-secret-123", 401)
	}))
	defer srv.Close()
	h := &HTTPModel{Base: srv.URL, Key: "sk-secret-123", Model: "m"}
	_, _, err := h.Chat(context.Background(), nil, nil)
	if err == nil || strings.Contains(err.Error(), "sk-secret-123") {
		t.Fatalf("want an error that does not contain the key: %v", err)
	}
}

func TestTheCopyHoldsOnlyMarkdownDocuments(t *testing.T) {
	root := memRoot(t)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(root, "link.md"))
	cp, err := copyMemory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cp)
	var got []string
	filepath.WalkDir(cp, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(cp, p)
			got = append(got, rel)
		}
		return nil
	})
	if len(got) != 2 || got[0] != "family.md" || got[1] != "interests.md" {
		t.Fatalf("copy holds %v, want only family.md and interests.md", got)
	}
}

type broken struct{ n int }

func (b *broken) Chat(context.Context, []Message, []ToolDef) (Message, Usage, error) {
	b.n++
	return Message{}, Usage{}, fmt.Errorf("model call failed: HTTP 400")
}

func TestARunThatCannotReachTheModelStopsAfterThreeFailures(t *testing.T) {
	var ps []memtest.Probe
	for i := 0; i < 10; i++ {
		ps = append(ps, memtest.Probe{ID: fmt.Sprint("p", i), Kind: "negative", Question: "q", Expected: "e"})
	}
	b := &broken{}
	r := Run(context.Background(), memRoot(t), memtest.Report{Probes: ps}, b, Options{})
	if b.n != 3 {
		t.Fatalf("made %d calls, want 3", b.n)
	}
	for _, p := range r.Probes[3:] {
		if !strings.Contains(p.Err, "stopped") {
			t.Fatalf("a skipped probe must say why: %+v", p)
		}
	}
}

func TestTheJudgeCanBeADifferentModelFromTheOneTested(t *testing.T) {
	tested := &script{replies: []Message{call("c1", "memory_get", `{"path":"interests"}`), say("Books.")}}
	judge := &script{replies: []Message{say("YES")}}
	r := Run(context.Background(), memRoot(t), memtest.Report{
		Facts:  []memtest.Fact{{ID: idA, Path: "interests", Text: "Loves reading books."}},
		Probes: []memtest.Probe{probe(idA, "direct")},
	}, tested, Options{Judge: true, JudgeModel: judge})
	if judge.i != 1 || tested.i != 2 {
		t.Fatalf("the judge made %d call(s) and the tested model %d; want 1 and 2", judge.i, tested.i)
	}
	if r.Probes[0].Judged == nil || !*r.Probes[0].Judged {
		t.Fatalf("%+v", r.Probes[0])
	}
}
