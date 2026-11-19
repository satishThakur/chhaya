package dream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/satishthakur/chhaya/internal/memrun"
)

type fakeGW struct {
	docs     map[string]string
	proposed []Candidate
	err      error
	gets     []string
}

func (f *fakeGW) List(context.Context) ([]string, error) {
	var out []string
	for k := range f.docs {
		out = append(out, k)
	}
	return out, nil
}
func (f *fakeGW) Get(_ context.Context, p string) (string, error) {
	f.gets = append(f.gets, p)
	d, ok := f.docs[p]
	if !ok {
		return "", errors.New("no such document")
	}
	return d, nil
}
func (f *fakeGW) Propose(_ context.Context, path, entry, rationale string) error {
	if f.err != nil {
		return f.err
	}
	f.proposed = append(f.proposed, Candidate{Path: path, Entry: entry, Rationale: rationale})
	return nil
}

// script plays back model turns: each is a set of tool calls, or text when it has none.
type script struct {
	turns [][]memrun.ToolCall
	text  string
	n     int
	seen  [][]memrun.Message
}

func (s *script) Chat(_ context.Context, msgs []memrun.Message, _ []memrun.ToolDef) (memrun.Message, memrun.Usage, error) {
	s.seen = append(s.seen, append([]memrun.Message{}, msgs...))
	if s.n >= len(s.turns) {
		return memrun.Message{Role: "assistant", Content: s.text}, memrun.Usage{In: 10, Out: 2}, nil
	}
	t := s.turns[s.n]
	s.n++
	return memrun.Message{Role: "assistant", ToolCalls: t}, memrun.Usage{In: 10, Out: 2}, nil
}

func tc(id, name string, args any) memrun.ToolCall {
	b, _ := json.Marshal(args)
	return memrun.ToolCall{ID: id, Name: name, Args: string(b)}
}

func gw() *fakeGW {
	return &fakeGW{docs: map[string]string{
		"interests": "# Interests\n\n- Enjoys the Baz series. <!-- proposal 20261101-aaaaaaaaaaaaaaaa, approved 2026-11-01 -->\n",
	}}
}

func TestAgentProposesThroughTheGatewayWithProvenanceBuiltByCode(t *testing.T) {
	g := gw()
	m := &script{turns: [][]memrun.ToolCall{
		{tc("1", "memory_get", map[string]any{"path": "interests"})},
		{tc("2", "propose_fact", map[string]any{"path": "interests", "entry": "Has already heard Foo and Bar.", "rationale": "said so", "source": []int64{4}})},
	}, text: "done"}
	r, err := RunAgent(context.Background(), m, g, in().Trace, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 1 || len(r.Proposed) != 1 {
		t.Fatalf("%+v %+v", g.proposed, r)
	}
	got := g.proposed[0].Rationale
	if !strings.Contains(got, "said so") || !strings.Contains(got, "trace t1") || !strings.Contains(got, "msg 4") {
		t.Fatalf("provenance must name the trace and the message, built by code: %q", got)
	}
	if r.Usage.In == 0 {
		t.Fatal("usage not counted")
	}
}

func TestAgentRefusesWhatFilterRefusesBeforeAnythingReachesTheGateway(t *testing.T) {
	g := gw()
	bad := func(id string, a map[string]any) memrun.ToolCall { return tc(id, "propose_fact", a) }
	m := &script{turns: [][]memrun.ToolCall{{
		bad("1", map[string]any{"path": "interests", "entry": "Owner PIN is 1234.", "rationale": "tool said", "source": []int64{3}}),
		bad("2", map[string]any{"path": "interests", "entry": "Likes jazz.", "rationale": "assistant said", "source": []int64{2}}),
		bad("3", map[string]any{"path": "interests", "entry": "Enjoys the Baz series.", "rationale": "repeat", "source": []int64{4}}),
		bad("4", map[string]any{"path": "nowhere", "entry": "Likes tea.", "rationale": "x", "source": []int64{4}}),
	}}, text: "done"}
	r, err := RunAgent(context.Background(), m, g, in().Trace, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 0 || len(r.Refused) != 4 {
		t.Fatalf("proposed %v, refused %d", g.proposed, len(r.Refused))
	}
	// The model is told why, so it can correct itself.
	var toolText string
	for _, msg := range m.seen[len(m.seen)-1] {
		if msg.Role == "tool" {
			toolText += msg.Content + "\n"
		}
	}
	for _, want := range []string{"not a message the owner typed", "duplicate", "no such document"} {
		if !strings.Contains(toolText, want) {
			t.Errorf("the model was not told %q:\n%s", want, toolText)
		}
	}
}

func TestAgentStopsAtTheCapAndNeverSendsMore(t *testing.T) {
	g := gw()
	var calls []memrun.ToolCall
	for i, e := range []string{"Fact one.", "Fact two.", "Fact three."} {
		calls = append(calls, tc(string(rune('a'+i)), "propose_fact", map[string]any{"path": "interests", "entry": e, "rationale": "x", "source": []int64{4}}))
	}
	r, err := RunAgent(context.Background(), &script{turns: [][]memrun.ToolCall{calls}, text: "done"}, g, in().Trace, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 2 || r.OverCap != 1 {
		t.Fatalf("proposed %d, over cap %d", len(g.proposed), r.OverCap)
	}
}

func TestAContradictionIsReportedAndNotSent(t *testing.T) {
	g := gw()
	m := &script{turns: [][]memrun.ToolCall{{tc("1", "report_contradiction", map[string]any{
		"path": "interests", "entry": "No longer enjoys the Baz series.", "contradicts": "20261101-aaaaaaaaaaaaaaaa", "rationale": "reversed", "source": []int64{4}})}}, text: "done"}
	r, err := RunAgent(context.Background(), m, g, in().Trace, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.proposed) != 0 || len(r.Contradictions) != 1 {
		t.Fatalf("a contradiction must be shown to the owner, never added: %v %+v", g.proposed, r)
	}
}

func TestAGatewayRefusalIsReportedNotHidden(t *testing.T) {
	g := gw()
	g.err = errors.New("blocked: the dreamer has made 5 proposal(s) today")
	m := &script{turns: [][]memrun.ToolCall{{tc("1", "propose_fact", map[string]any{"path": "interests", "entry": "Likes tea.", "rationale": "x", "source": []int64{4}})}}, text: "done"}
	r, err := RunAgent(context.Background(), m, g, in().Trace, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Proposed) != 0 || len(r.Refused) != 1 || !strings.Contains(r.Refused[0].Reason, "blocked") {
		t.Fatalf("%+v", r)
	}
}

func TestAgentStopsAfterTheStepLimitAndOnAModelError(t *testing.T) {
	loop := &script{text: "x"}
	for i := 0; i < 40; i++ {
		loop.turns = append(loop.turns, []memrun.ToolCall{tc("l", "memory_list", map[string]any{})})
	}
	r, err := RunAgent(context.Background(), loop, gw(), in().Trace, 5)
	if err == nil || r.Steps > maxAgentSteps {
		t.Fatalf("a model that never finishes must stop: steps %d err %v", r.Steps, err)
	}
}

func TestIneligibleTraceIsNotRead(t *testing.T) {
	tr := in().Trace
	tr.Source = "cron"
	m := &script{text: "done"}
	if _, err := RunAgent(context.Background(), m, gw(), tr, 5); err == nil || len(m.seen) != 0 {
		t.Fatalf("a cron trace must not reach the model: %v", err)
	}
}
