package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/satishthakur/chhaya/internal/audit"
	"github.com/satishthakur/chhaya/internal/memory"
	"github.com/satishthakur/chhaya/internal/state"
)

const foodDoc = "# Food\n\n- likes dosa\n"

// memBot is a bot whose memory root holds taste/food.md.
func memBot(t *testing.T) (*Bot, *fakeTG, *state.Store, string, string) {
	t.Helper()
	tg := newFakeTG()
	b, st, logPath := newTestBot(t, tg, time.Second)
	if err := os.MkdirAll(filepath.Join(b.mem.Root, "taste"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(b.mem.Root, "taste", "food.md")
	if err := os.WriteFile(doc, []byte(foodDoc), 0o664); err != nil {
		t.Fatal(err)
	}
	return b, tg, st, logPath, doc
}

// propose stages a proposal and posts its card, returning the card's message id.
func propose(t *testing.T, b *Bot, tg *fakeTG, entry, rationale string) (memory.Proposal, int64) {
	t.Helper()
	p, err := b.mem.Stage("taste/food", entry, rationale, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ProposeMemory(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p, int64(len(tg.texts()))
}

// memTap is a callback from the approver carrying the token for msgID.
func memTap(msgID int64, verb string) Update {
	return Update{ID: time.Now().UnixNano(), Callback: &Callback{
		ID: fmt.Sprintf("cb-%s-%d-%d", verb, msgID, time.Now().UnixNano()), From: User{ID: allowedUser},
		Data:    verb + ":" + callbackToken(msgID),
		Message: &Message{ID: msgID, Chat: Chat{ID: allowedUser}},
	}}
}

func readDoc(t *testing.T, doc string) string {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isPending(t *testing.T, b *Bot, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(b.mem.Root, ".proposals", id+".json"))
	return err == nil
}

func (f *fakeTG) edit(id int64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.edits[id]
	return s, ok
}

func (f *fakeTG) lastAnswer() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answerTexts) == 0 {
		return ""
	}
	return f.answerTexts[len(f.answerTexts)-1]
}

func lastRecord(t *testing.T, logPath string) audit.Record {
	t.Helper()
	recs := records(t, logPath)
	if len(recs) == 0 {
		t.Fatal("no audit record was written")
	}
	return recs[len(recs)-1]
}

func TestProposeMemoryPostsTheCard(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	propose(t, b, tg, "No okra.", "said so")

	card := tg.texts()[0]
	for _, want := range []string{"approve memory?", "<code>taste/food</code>: No okra.", "why: said so"} {
		if !strings.Contains(card, want) {
			t.Errorf("card does not contain %q:\n%s", want, card)
		}
	}
}

// The entry and the rationale are agent text on an HTML card the human
// approves on the strength of.
func TestTheMemoryCardEscapesAgentText(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	propose(t, b, tg, `No "masala" & no <b>okra</b>`, "</code><b>forged</b>")

	card := tg.texts()[0]
	want := []string{"<b>", "</b>", "<code>", "</code>"}
	if got := tagSequence(card); !slices.Equal(got, want) {
		t.Errorf("agent text changed the card's markup.\n got: %v\nwant: %v\ncard:\n%s", got, want, card)
	}
	if !strings.Contains(card, "&lt;b&gt;okra&lt;/b&gt;") || !strings.Contains(card, "&amp;") {
		t.Errorf("the entry should be escaped, not dropped:\n%s", card)
	}
}

func TestApproveTapWritesAndEditsTheCard(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if !strings.Contains(readDoc(t, doc), "- No okra. <!-- proposal "+p.ID+",") {
		t.Fatalf("the line was not written:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "✅ written to") {
		t.Errorf("edit = %q", e)
	}
	r := lastRecord(t, logPath)
	if r.Tool != "memory.apply" || r.Decision != "allow" || r.Rule != "memory.tap_approved" ||
		r.Actor != "approver" || r.Kind != "write" || r.Tier != 0 {
		t.Errorf("record = %+v", r)
	}
	if r.Approval == nil || r.Approval.Answer != "approved" {
		t.Errorf("approval = %+v", r.Approval)
	}
	if strings.Contains(string(r.Args), "entry") || strings.Contains(string(r.Args), "okra") ||
		!strings.Contains(string(r.Args), p.ID) || !strings.Contains(string(r.Args), "taste/food") {
		t.Errorf("args = %s, want proposal_id and path, never the entry", r.Args)
	}
}

func TestRejectTapWritesNothing(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	b.handle(context.Background(), memTap(msgID, "mem-reject"))

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a reject touched the document:\n%s", readDoc(t, doc))
	}
	if isPending(t, b, p.ID) {
		t.Error("a rejected proposal is still pending")
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "❌ rejected") {
		t.Errorf("edit = %q", e)
	}
	r := lastRecord(t, logPath)
	if r.Tool != "memory.apply" || r.Decision != "deny" || r.Rule != "memory.tap_rejected" {
		t.Errorf("record = %+v", r)
	}
	if r.Approval == nil || r.Approval.Answer != "denied_by_user" {
		t.Errorf("approval = %+v", r.Approval)
	}
}

func TestAForgedMemoryTapWritesNothing(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	u := memTap(msgID, "mem-approve")
	u.Callback.Data = "mem-approve:" + callbackToken(msgID+1)
	b.handle(context.Background(), u)

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a forged tap wrote:\n%s", readDoc(t, doc))
	}
	if !isPending(t, b, p.ID) {
		t.Error("a forged tap resolved the proposal")
	}
}

func TestAMemoryTapFromAnotherUserWritesNothing(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	u := memTap(msgID, "mem-approve")
	u.Callback.From.ID = allowedUser + 1
	b.handle(context.Background(), u)

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a stranger's tap wrote:\n%s", readDoc(t, doc))
	}
	if !isPending(t, b, p.ID) {
		t.Error("a stranger's tap resolved the proposal")
	}
	r := lastRecord(t, logPath)
	if r.Rule != "approver.unauthorized_sender" || r.Severity != "critical" {
		t.Errorf("record = %+v", r)
	}
}

func TestASecondTapIsAlreadyAnswered(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	_, msgID := propose(t, b, tg, "No okra.", "said so")

	b.handle(context.Background(), memTap(msgID, "mem-approve"))
	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if n := strings.Count(readDoc(t, doc), "No okra."); n != 1 {
		t.Errorf("the line appears %d times, want 1", n)
	}
	if a := tg.lastAnswer(); a != "already answered" {
		t.Errorf("second answer = %q", a)
	}
}

func TestApproveWhileStoppedWritesNothingThenWorksAfterResume(t *testing.T) {
	b, tg, st, logPath, doc := memBot(t)
	_, msgID := propose(t, b, tg, "No okra.", "said so")

	if err := st.SetKillSwitch(true, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a tap wrote while stopped:\n%s", readDoc(t, doc))
	}
	r := lastRecord(t, logPath)
	if r.Decision != "deny" || r.Rule != "killswitch.active" || r.Tool != "memory.apply" {
		t.Errorf("record = %+v", r)
	}
	if a := tg.lastAnswer(); !strings.Contains(a, "kill switch") {
		t.Errorf("answer = %q, want the policy's own reason", a)
	}
	if e, ok := tg.edit(msgID); ok {
		t.Errorf("a refused tap edited the card to %q; it must stay tappable for after /resume", e)
	}

	if err := st.SetKillSwitch(false, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if n := strings.Count(readDoc(t, doc), "No okra."); n != 1 {
		t.Errorf("after /resume the line appears %d times, want 1", n)
	}
}

// The tap is the approval that taint would ask for, so taint must not block it.
func TestTaintDoesNotBlockAnApproveTap(t *testing.T) {
	b, tg, st, _, doc := memBot(t)
	_, msgID := propose(t, b, tg, "No okra.", "said so")

	if err := st.SetTaint(true, "web.fetch", time.Now()); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if !strings.Contains(readDoc(t, doc), "No okra.") {
		t.Fatalf("taint blocked an approve tap:\n%s", readDoc(t, doc))
	}
}

func TestAMissingProposalFileEditsTheCardToNotWritten(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")
	if err := b.mem.Remove(p.ID); err != nil {
		t.Fatal(err)
	}

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a missing proposal wrote:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") {
		t.Errorf("edit = %q", e)
	}
	if r := lastRecord(t, logPath); r.Result.Status != "error" || r.Result.Error == "" {
		t.Errorf("record = %+v, want a result error", r)
	}
}

func TestRepostPendingPostsReadyAndShelvesLegacy(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	if _, err := b.mem.Stage("taste/food", "No okra.", "said so", time.Now()); err != nil {
		t.Fatal(err)
	}
	const legacyID = "20261001-0123456789abcdef"
	legacy := filepath.Join(b.mem.Root, ".proposals", legacyID+".json")
	body := `{"proposal_id":"` + legacyID + `","path":"taste/food","diff":"+ no okra","rationale":"old"}`
	if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.RepostPending(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := len(tg.texts()); n != 1 {
		t.Errorf("%d cards sent, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(b.mem.Root, ".proposals", "legacy", legacyID+".json")); err != nil {
		t.Errorf("the legacy proposal was not shelved: %v", err)
	}
}

// TestOnlyTheTapHandlerApplies: the approver's tap is the only thing that may
// write a memory document. A second caller of ApplyApproved — a tool, an MCP
// handler, a helper — would be a write path with no human tap behind it.
// The walk mirrors TestNoPathToAHandlerAnywhereInTheModule in mcpsrv; it is
// copied, not imported, because test code does not cross packages.
func TestOnlyTheTapHandlerApplies(t *testing.T) {
	root := moduleRoot(t)

	// path (relative to the module root) → the only function in it that may
	// call ApplyApproved. An empty function name means the whole file may.
	allowed := map[string]string{
		"internal/approval/memorycard.go": "memoryTap",
		"internal/memory/memory_test.go":  "", // unit-tests ApplyApproved itself
	}

	var parsed int
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			if name := de.Name(); name == ".git" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		parsed++

		for _, u := range applyApprovedUses(f) {
			if want, ok := allowed[filepath.ToSlash(rel)]; ok && (want == "" || want == u.fn) {
				continue
			}
			t.Errorf("%s:%d: %s uses ApplyApproved; only the approver's memoryTap may",
				rel, fset.Position(u.pos).Line, u.fn)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that silently matched nothing would be the most comfortable way
	// for this test to go green.
	if parsed < 20 {
		t.Fatalf("only %d Go files were walked; the walk is not covering the module", parsed)
	}
}

type applyUse struct {
	fn  string // the enclosing function, or "" at package level
	pos token.Pos
}

// applyApprovedUses finds every selector naming ApplyApproved, called or
// not. Matching only call expressions would miss a method value taken here
// and called somewhere the walk does not attribute to it.
func applyApprovedUses(f *ast.File) []applyUse {
	var uses []applyUse
	ast.Inspect(f, func(n ast.Node) bool {
		s, ok := n.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "ApplyApproved" {
			return true
		}
		fn := ""
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= s.Pos() && s.Pos() <= fd.End() {
				fn = fd.Name.Name
			}
		}
		uses = append(uses, applyUse{fn: fn, pos: s.Sel.Pos()})
		return true
	})
	return uses
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// A rationale in a non-Latin script must not be cut mid-character: Telegram
// refuses text that is not valid UTF-8, so the card would never be posted.
func TestALongNonLatinRationaleStaysValidUTF8(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	propose(t, b, tg, "No okra.", "a"+strings.Repeat("भ", 200))

	if card := tg.texts()[0]; !utf8.ValidString(card) {
		t.Errorf("the card is not valid UTF-8:\n%q", card)
	}
}

// buttonsOn returns the verbs on msgID's keyboard, failing the test if any
// button carries a token that would not pass tap()'s check.
func buttonsOn(t *testing.T, tg *fakeTG, msgID int64) []string {
	t.Helper()
	tg.mu.Lock()
	defer tg.mu.Unlock()
	var verbs []string
	for _, row := range tg.keyboards[msgID] {
		for _, btn := range row {
			verb, tok, _ := strings.Cut(btn.Data, ":")
			if tok != callbackToken(msgID) {
				t.Errorf("button %q carries a token for another message", btn.Data)
			}
			verbs = append(verbs, verb)
		}
	}
	return verbs
}

func TestAnAnsweredCardLosesItsButtons(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	_, msgID := propose(t, b, tg, "No okra.", "said so")
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Fatalf("precondition: card buttons = %v", got)
	}

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if got := buttonsOn(t, tg, msgID); len(got) != 0 {
		t.Errorf("a written card still has buttons %v", got)
	}
}

// A write that failed for a reason the human can fix must leave a card they
// can tap again, not one that says "not written" and offers nothing.
func TestAFailedApproveKeepsTheButtonsAndCanBeRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes, so the write cannot be made to fail")
	}
	b, tg, _, _, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")
	if err := os.Chmod(doc, 0o444); err != nil {
		t.Fatal(err)
	}

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a failed write changed the document:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") {
		t.Errorf("edit = %q", e)
	}
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Errorf("after a failed write the card has buttons %v, want both back", got)
	}
	if !isPending(t, b, p.ID) {
		t.Error("a failed write resolved the proposal")
	}

	if err := os.Chmod(doc, 0o664); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if n := strings.Count(readDoc(t, doc), "No okra."); n != 1 {
		t.Errorf("after the retry the line appears %d times, want 1", n)
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "✅ written to") {
		t.Errorf("retry edit = %q", e)
	}
}

func TestAFailedRejectKeepsTheButtonsAndCanBeRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes, so the reject cannot be made to fail")
	}
	b, tg, _, _, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")
	dir := filepath.Join(b.mem.Root, ".proposals")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	b.handle(context.Background(), memTap(msgID, "mem-reject"))

	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") {
		t.Errorf("edit = %q", e)
	}
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Errorf("after a failed reject the card has buttons %v, want both back", got)
	}
	if !isPending(t, b, p.ID) {
		t.Error("a failed reject resolved the proposal")
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-reject"))

	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "❌ rejected") {
		t.Errorf("retry edit = %q", e)
	}
	if isPending(t, b, p.ID) {
		t.Error("the retried reject left the proposal pending")
	}
	if readDoc(t, doc) != foodDoc {
		t.Errorf("a reject touched the document:\n%s", readDoc(t, doc))
	}
}

// What the human approves must be what lands in the file. A proposal edited
// on disk after its card went out is refused, not applied.
func TestAProposalChangedAfterTheCardWritesNothing(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	changed := p
	changed.Entry = "Okra is fine."
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.mem.Root, ".proposals", p.ID+".json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	if readDoc(t, doc) != foodDoc {
		t.Fatalf("a changed proposal was applied:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") || !strings.Contains(e, "changed after this card was shown") {
		t.Errorf("edit = %q", e)
	}
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Errorf("card buttons = %v, want both kept", got)
	}
	if !isPending(t, b, p.ID) {
		t.Error("a changed proposal was resolved")
	}
	r := lastRecord(t, logPath)
	if r.Tool != "memory.apply" || r.Result.Status != "error" || !strings.Contains(r.Result.Error, "changed") {
		t.Errorf("record = %+v, want the refusal recorded as an error", r)
	}
}

// The write is preceded by a record of the intent to write, so a document
// never changes without the ledger saying so first; the record after the
// write says how it went.
func TestAnApproveTapRecordsItsIntentBeforeWriting(t *testing.T) {
	b, tg, _, logPath, _ := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	recs := records(t, logPath)
	if len(recs) < 2 {
		t.Fatalf("%d records, want an intent and a result", len(recs))
	}
	intent, done := recs[len(recs)-2], recs[len(recs)-1]
	for _, r := range []audit.Record{intent, done} {
		if r.Tool != "memory.apply" || r.Decision != "allow" || r.Rule != "memory.tap_approved" ||
			r.Approval == nil || r.Approval.Answer != "approved" || !strings.Contains(string(r.Args), p.ID) {
			t.Errorf("record = %+v", r)
		}
	}
	if intent.Result.Status != "pending" || done.Result.Status != "ok" {
		t.Errorf("statuses = %q then %q, want pending then ok", intent.Result.Status, done.Result.Status)
	}
}

// brokenAudit closes the bot's audit writer, so every append fails.
func brokenAudit(t *testing.T, b *Bot) {
	t.Helper()
	if err := b.audit.Close(); err != nil {
		t.Fatal(err)
	}
}

// refusedUnwritten asserts a tap left everything as it was: the document,
// the pending proposal, and a ⚠️ card that still has both buttons.
func refusedUnwritten(t *testing.T, b *Bot, tg *fakeTG, msgID int64, id, doc, wantDoc string) {
	t.Helper()
	if got := readDoc(t, doc); got != wantDoc {
		t.Errorf("the document changed:\n%s", got)
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") {
		t.Errorf("edit = %q", e)
	}
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Errorf("card buttons = %v, want both kept", got)
	}
	if !isPending(t, b, id) {
		t.Error("the proposal was resolved")
	}
}

// A write the ledger cannot record is not made: if the intent record does
// not land, the document is not touched.
func TestAnApproveTapWithABrokenAuditLogWritesNothing(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")
	brokenAudit(t, b)

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	refusedUnwritten(t, b, tg, msgID, p.ID, doc, foodDoc)
}

// Once any approver audit append has failed, the ledger has a gap, and no
// later tap — approve or reject — may act, even if the log would now take
// the record.
func TestAfterAFailedAuditAppendEveryTapIsRefused(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	first, firstMsg := propose(t, b, tg, "No okra.", "said so")
	second, secondMsg := propose(t, b, tg, "No karela.", "said so")

	// The reject moves its file and then cannot record it: the latch is set.
	brokenAudit(t, b)
	b.handle(context.Background(), memTap(firstMsg, "mem-reject"))
	if isPending(t, b, first.ID) {
		t.Fatal("precondition: the first reject should have gone through")
	}

	// A working log again. The latch, not the log, must refuse.
	w, err := audit.Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	b.audit = w

	b.handle(context.Background(), memTap(secondMsg, "mem-approve"))
	refusedUnwritten(t, b, tg, secondMsg, second.ID, doc, foodDoc)

	b.handle(context.Background(), memTap(secondMsg, "mem-reject"))
	refusedUnwritten(t, b, tg, secondMsg, second.ID, doc, foodDoc)
}

// A crash between the append and the move leaves the line written and the
// proposal pending. Reject must not file a written line as rejected; the
// card says approve finishes it, and approve does.
func TestRejectAfterACrashedApproveSaysApproveToFinish(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	p, msgID := propose(t, b, tg, "No okra.", "said so")
	f, err := os.OpenFile(doc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("- No okra. <!-- proposal " + p.ID + ", approved 2026-10-30 -->\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	written := readDoc(t, doc)

	b.handle(context.Background(), memTap(msgID, "mem-reject"))

	refusedUnwritten(t, b, tg, msgID, p.ID, doc, written)
	if e, _ := tg.edit(msgID); e != "⚠️ not written: already written; approve to finish filing it" {
		t.Errorf("edit = %q", e)
	}

	b.handle(context.Background(), memTap(msgID, "mem-approve"))
	if n := strings.Count(readDoc(t, doc), p.ID); n != 1 {
		t.Errorf("the line appears %d times, want 1", n)
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "✅ written to") {
		t.Errorf("edit = %q", e)
	}
}

func TestRepostPendingPostsTheGoodOnesPastACorruptFile(t *testing.T) {
	b, tg, _, _, _ := memBot(t)
	if _, err := b.mem.Stage("taste/food", "No okra.", "said so", time.Now()); err != nil {
		t.Fatal(err)
	}
	const legacyID = "20261001-0123456789abcdef"
	const corruptID = "20261001-0000000000000000"
	dir := filepath.Join(b.mem.Root, ".proposals")
	if err := os.WriteFile(filepath.Join(dir, legacyID+".json"),
		[]byte(`{"proposal_id":"`+legacyID+`","path":"taste/food","diff":"+ no okra","rationale":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, corruptID+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := b.RepostPending(context.Background())

	if err == nil || !strings.Contains(err.Error(), corruptID) {
		t.Errorf("err = %v, want the corrupt file reported", err)
	}
	if n := len(tg.texts()); n != 1 {
		t.Errorf("%d cards sent, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "legacy", legacyID+".json")); err != nil {
		t.Errorf("the legacy proposal was not shelved: %v", err)
	}
}

// Telegram refuses an answerCallbackQuery text over 200 characters, so a
// long refusal reason would leave the tap spinning with no answer at all.
func TestATapAnswerFitsTelegramsLimit(t *testing.T) {
	long := strings.Repeat("भ", 150) + strings.Repeat("x", 150)
	got := tapAnswer(long)
	if n := utf8.RuneCountInString(got); n > 200 {
		t.Errorf("answer is %d characters, want at most 200", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("answer is not valid UTF-8: %q", got)
	}
	if short := "blocked: kill switch active"; tapAnswer(short) != short {
		t.Errorf("a short reason was changed: %q", tapAnswer(short))
	}
}

// The walk must see a method value, not only a call: `f := s.ApplyApproved`
// followed by f(...) is a second write path all the same.
func TestApplyApprovedUsesSeesMethodValues(t *testing.T) {
	src := `package x
func a(s S) { s.ApplyApproved("id", now) }
func b(s S) { f := s.ApplyApproved; f("id", now) }
func c(s S) { run(s.ApplyApproved) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fns []string
	for _, u := range applyApprovedUses(f) {
		fns = append(fns, u.fn)
	}
	if !slices.Equal(fns, []string{"a", "b", "c"}) {
		t.Errorf("uses found in %v, want [a b c]", fns)
	}
}
