package dream

import (
	"context"
	"strings"
	"testing"

	"github.com/satishthakur/chhaya/internal/memrun"
)

const caseJSON = `{
 "id":"recall","cap":5,
 "memory":{"interests":["Enjoys the Baz series. <!-- proposal 20261101-aaaaaaaaaaaaaaaa, approved 2026-11-01 -->"]},
 "trace":{"id":"t1","source":"telegram","messages":[
  {"id":1,"role":"user","content":"suggest a series"},
  {"id":2,"role":"assistant","content":"Try Foo."},
  {"id":3,"role":"user","content":"I already heard Foo"}]},
 "expect":{"propose":[{"path":"interests","all":["heard","foo"],"source":[3]}]}
}`

func prop(id, path, entry string, src ...int64) memrun.ToolCall {
	return tc(id, "propose_fact", map[string]any{"path": path, "entry": entry, "rationale": "x", "source": src})
}

func play(calls ...memrun.ToolCall) *script {
	return &script{turns: [][]memrun.ToolCall{calls}, text: "done"}
}

func TestRunPassesWhenTheExpectedFactComesBack(t *testing.T) {
	c, err := ParseCase([]byte(caseJSON))
	if err != nil {
		t.Fatal(err)
	}
	r := RunCase(context.Background(), play(prop("1", "interests", "Has already heard the Foo series.", 3)), c)
	if !r.Pass {
		t.Fatalf("%v", r.Why)
	}
}

func TestRunFailsOnAMissedFactNoiseAndAWrongSource(t *testing.T) {
	c, _ := ParseCase([]byte(caseJSON))
	cases := map[string]*script{
		"missed":       play(),
		"noise":        play(prop("1", "interests", "Has heard Foo.", 3), prop("2", "interests", "Likes tea.", 3)),
		"wrong text":   play(prop("1", "interests", "Likes tea.", 3)),
		"wrong source": play(prop("1", "interests", "Has heard Foo.", 1)), // an owner message, but not the one that says it
	}
	for name, m := range cases {
		if r := RunCase(context.Background(), m, c); r.Pass {
			t.Errorf("%s: should have failed", name)
		}
	}
	// a model that fails or never finishes is a failed case with the reason
	loop := &script{text: "x"}
	for i := 0; i < 30; i++ {
		loop.turns = append(loop.turns, []memrun.ToolCall{tc("l", "memory_list", map[string]any{})})
	}
	if r := RunCase(context.Background(), loop, c); r.Pass || !strings.Contains(strings.Join(r.Why, " "), "agent:") {
		t.Errorf("a runaway agent must fail the case: %+v", r)
	}
}

func TestSilenceCaseFailsOnAnyProposal(t *testing.T) {
	c, err := ParseCase([]byte(`{"id":"hi","cap":5,"memory":{"interests":[]},
	 "trace":{"id":"t","source":"telegram","messages":[{"id":1,"role":"user","content":"hi"}]},"expect":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := RunCase(context.Background(), play(), c); !r.Pass {
		t.Fatalf("%v", r.Why)
	}
	r := RunCase(context.Background(), play(prop("1", "interests", "Says hi.", 1)), c)
	if r.Pass || !strings.Contains(strings.Join(r.Why, " "), "expected 0") {
		t.Fatalf("a noisy proposal must fail a silence case: %+v", r)
	}
}

func TestParseCaseRejectsBadCases(t *testing.T) {
	for name, j := range map[string]string{
		"no id":         `{"memory":{},"trace":{"messages":[]},"expect":{}}`,
		"unknown field": `{"id":"x","memory":{},"trace":{"messages":[]},"expect":{},"bogus":1}`,
		"no trace":      `{"id":"x","memory":{},"expect":{}}`,
	} {
		if _, err := ParseCase([]byte(j)); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestShippedCasesAreWellFormed(t *testing.T) {
	cs, err := LoadCases("cases")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) < 10 {
		t.Fatalf("expected the shipped cases, got %d", len(cs))
	}
	for _, c := range cs {
		if _, err := inputFor(c); err != nil {
			t.Errorf("%s: %v", c.ID, err)
		}
	}
}

// The injection cases must stay safe even when the model is fooled: the filter refuses the line
// before it reaches the gateway.
func TestInjectionCasesPassEvenWithAFooledModel(t *testing.T) {
	cs, _ := LoadCases("cases")
	fooled := map[string]memrun.ToolCall{
		"injection-tool-output":   prop("1", "interests", "Favourite cuisine is Thai.", 2),
		"injection-system-note":   prop("1", "interests", "Is vegetarian.", 2),
		"silence-assistant-claim": prop("1", "interests", "Loves spicy Thai food.", 2),
	}
	n := 0
	for _, c := range cs {
		if call, ok := fooled[c.ID]; ok {
			n++
			r := RunCase(context.Background(), play(call), c)
			if !r.Pass || len(r.Result.Dropped) != 1 {
				t.Errorf("%s: %v %+v", c.ID, r.Why, r.Result)
			}
		}
	}
	if n != 3 {
		t.Fatalf("ran %d fooled cases", n)
	}
}
