package dream

import (
	"strings"
	"testing"
	"time"
)

func sampleSummary() Summary {
	return Summary{Read: 3, Skipped: 2, Results: []TraceResult{
		{ID: "t-prop", Day: "2026-11-16", Messages: 12,
			Proposed: []Candidate{{Path: "interests", Entry: "Has already heard <b>Foo</b>.", Rationale: "said so (trace t-prop, msg 7)", Source: []int64{7}}},
			Quotes:   map[int64]string{7: "I already heard <script>alert(1)</script> Foo"}},
		{ID: "t-contra", Day: "2026-11-16", Messages: 5,
			Contradictions: []Candidate{{Path: "interests", Entry: "Watches only Malayalam movies now.", Contradicts: "20261109-0abc00000000f778", Source: []int64{3}}},
			Quotes:         map[int64]string{3: "not any more, only Malayalam"}},
		{ID: "t-none", Day: "2026-11-15", Messages: 4},
		{ID: "t-err", Day: "2026-11-15", Messages: 4, Err: "the agent did not finish"},
		{ID: "t-ref", Day: "2026-11-14", Messages: 6,
			Refused: []Drop{{Candidate: Candidate{Path: "interests", Entry: "Likes tea.", Source: []int64{2}}, Reason: "source is not a message the owner typed"}}},
	}}
}

func TestReviewNumbersEveryProposalAndQuotesTheOwner(t *testing.T) {
	h := ReviewHTML(sampleSummary(), time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	for _, want := range []string{"Would propose (1)", "#1", "t-prop", "msg 7", "Contradictions (1)", "20261109-0abc00000000f778",
		"Refused (1)", "not a message the owner typed", "Nothing to save (2)", "Errors (1)", "the agent did not finish",
		"read 3", "skipped 2"} {
		if !strings.Contains(h, want) {
			t.Errorf("the review lacks %q", want)
		}
	}
}

func TestReviewEscapesEverythingTheConversationCanContain(t *testing.T) {
	h := ReviewHTML(sampleSummary(), time.Now())
	if strings.Contains(h, "<script>alert") || strings.Contains(h, "<b>Foo</b>") {
		t.Fatal("text from a conversation or from the model was not escaped")
	}
	if !strings.Contains(h, "&lt;script&gt;") {
		t.Fatal("the owner's words should still be shown, escaped")
	}
}

func TestReviewWithNothingProposedSaysSo(t *testing.T) {
	h := ReviewHTML(Summary{Read: 2, Results: []TraceResult{{ID: "a", Messages: 3}, {ID: "b", Messages: 3}}}, time.Now())
	if !strings.Contains(h, "Would propose (0)") || !strings.Contains(h, "Nothing to save (2)") {
		t.Fatal(h)
	}
}
