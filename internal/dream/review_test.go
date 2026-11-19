package dream

import (
	"errors"
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
		{ID: "t-none", Day: "2026-11-15", Messages: 4, First: "what is the weather"},
		{ID: "t-err", Day: "2026-11-15", Messages: 4, Err: "the agent did not finish"},
		{ID: "t-ref", Day: "2026-11-14", Messages: 6,
			Refused: []Drop{{Candidate: Candidate{Path: "interests", Entry: "Likes tea.", Source: []int64{2}}, Reason: "source is not a message the owner typed"}}},
	}}
}

func TestReviewNumbersEveryProposalAndQuotesTheOwner(t *testing.T) {
	h := ReviewHTML(sampleSummary(), time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	for _, want := range []string{"Would propose (1)", "#1", "t-prop", "msg 7", "Contradictions (1)", "20261109-0abc00000000f778",
		"Refused (1)", "not a message the owner typed", "Nothing to save (2)", "what is the weather", "Errors (1)", "the agent did not finish",
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

func TestSummaryMessageHasCountsAndNeverTheText(t *testing.T) {
	s := sampleSummary()
	s.Proposed, s.Contradictions, s.Refused, s.Errors = 1, 1, 1, 1
	m := SummaryMessage(s, true, "https://x/r", "2026-11-19", nil)
	for _, want := range []string{"2026-11-19", "FAILED", "read 3", "1 proposed", "1 contradiction", "1 refused", "1 error", "https://x/r"} {
		if !strings.Contains(m, want) {
			t.Errorf("summary lacks %q:\n%s", want, m)
		}
	}
	for _, leak := range []string{"Has already heard", "Foo", "Malayalam", "Likes tea", "I already heard"} {
		if strings.Contains(m, leak) {
			t.Errorf("the summary quotes %q; text from a conversation belongs only on the page", leak)
		}
	}
}

func TestSummaryHeadlines(t *testing.T) {
	clean := Summary{Read: 4}
	if m := SummaryMessage(clean, true, "", "d", nil); !strings.Contains(m, "nothing new") {
		t.Errorf("%s", m)
	}
	one := Summary{Read: 4, Proposed: 2}
	if m := SummaryMessage(one, true, "", "d", nil); !strings.Contains(m, "2 proposed") || !strings.Contains(m, "tap") {
		t.Errorf("%s", m)
	}
	if m := SummaryMessage(one, false, "", "d", nil); !strings.Contains(m, "dry run") || strings.Contains(m, "tap") {
		t.Errorf("a dry run sent nothing to tap: %s", m)
	}
	if m := SummaryMessage(clean, true, "", "d", errors.New("could not connect")); !strings.Contains(m, "FAILED") || !strings.Contains(m, "could not connect") {
		t.Errorf("%s", m)
	}
}
