package dream

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satishthakur/chhaya/internal/memrun"
)

type scripted struct {
	reply string
	err   error
	got   []memrun.Message
}

func (s *scripted) Chat(_ context.Context, msgs []memrun.Message, _ []memrun.ToolDef) (memrun.Message, memrun.Usage, error) {
	s.got = msgs
	return memrun.Message{Role: "assistant", Content: s.reply}, memrun.Usage{In: 10, Out: 5}, s.err
}

func TestExtractParsesJSONInsideFencesAndProse(t *testing.T) {
	m := &scripted{reply: "Here you go:\n```json\n{\"proposals\":[{\"path\":\"interests\",\"entry\":\"Has heard Foo.\",\"rationale\":\"said\",\"source\":[4]}],\"contradictions\":[]}\n```"}
	cs, u, err := Extract(context.Background(), m, in())
	if err != nil || len(cs) != 1 || cs[0].Entry != "Has heard Foo." || u.In != 10 {
		t.Fatalf("%+v %v %+v", cs, err, u)
	}
}

func TestExtractFailsClosedOnGarbageAndOnModelError(t *testing.T) {
	if cs, _, err := Extract(context.Background(), &scripted{reply: "I think the owner likes tea."}, in()); err == nil || len(cs) != 0 {
		t.Fatalf("prose with no JSON must be an error, not a proposal: %+v %v", cs, err)
	}
	if _, _, err := Extract(context.Background(), &scripted{reply: `{"proposals":"nope"}`}, in()); err == nil {
		t.Fatal("wrong shape must be an error")
	}
	if _, _, err := Extract(context.Background(), &scripted{err: errors.New("down")}, in()); err == nil {
		t.Fatal("a model error must be returned")
	}
}

func TestExtractLabelsWhoSaidWhat(t *testing.T) {
	m := &scripted{reply: `{"proposals":[],"contradictions":[]}`}
	if _, _, err := Extract(context.Background(), m, in()); err != nil {
		t.Fatal(err)
	}
	var all string
	for _, x := range m.got {
		all += x.Content + "\n"
	}
	for _, want := range []string{"[1] OWNER:", "[2] ASSISTANT:", "[3] TOOL OUTPUT", "[4] OWNER: I already heard Foo and Bar", "[5] SYSTEM NOTE", "interests", "20261101-aaaaaaaaaaaaaaaa", "Enjoys the Baz series."} {
		if !strings.Contains(all, want) {
			t.Errorf("prompt lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "old text") {
		t.Error("an inactive message must not be shown")
	}
	if strings.Contains(all, "[5] OWNER") || strings.Contains(all, "[3] OWNER") {
		t.Error("tool output or a system note was labelled as the owner")
	}
}
