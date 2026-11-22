package approval

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/memory"
)

const dupID = "20261030-aaaaaaaaaaaaaaaa"

const dupDoc = "# Food\n\n- likes dosa <!-- proposal 20261030-bbbbbbbbbbbbbbbb, approved 2026-10-30 -->\n" +
	"- Likes <dosa> & chai. <!-- proposal " + dupID + ", approved 2026-10-30 -->\n"

// removal stages a removal of dupID in taste/food and posts its card.
func removal(t *testing.T, b *Bot, tg *fakeTG, doc string) (memory.Proposal, int64) {
	t.Helper()
	if err := os.WriteFile(doc, []byte(dupDoc), 0o664); err != nil {
		t.Fatal(err)
	}
	p, err := b.mem.StageRemove("taste/food", dupID, "said twice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ProposeMemory(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p, int64(len(tg.texts()))
}

func TestARemovalCardShowsTheLineAndSaysRemove(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	_, msgID := removal(t, b, tg, doc)
	card := tg.texts()[msgID-1]
	if !strings.Contains(card, "REMOVING") || !strings.Contains(card, "<s>Likes &lt;dosa&gt; &amp; chai.</s>") {
		t.Fatalf("card = %q", card)
	}
	if got := buttonsOn(t, tg, msgID); !slices.Equal(got, []string{"mem-approve", "mem-reject"}) {
		t.Errorf("buttons = %v", got)
	}
}

func TestApprovingARemovalTakesOutThatLineAndRecordsWhich(t *testing.T) {
	b, tg, _, logPath, doc := memBot(t)
	p, msgID := removal(t, b, tg, doc)

	b.handle(context.Background(), memTap(msgID, "mem-approve"))

	got := readDoc(t, doc)
	if strings.Contains(got, dupID) || !strings.Contains(got, "likes dosa") {
		t.Fatalf("document:\n%s", got)
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "🗑 removed from") {
		t.Errorf("edit = %q", e)
	}
	r := lastRecord(t, logPath)
	if r.Tool != "memory.apply" || r.Decision != "allow" || r.Result.Status != "ok" || r.Actor != "approver" {
		t.Errorf("record = %+v", r)
	}
	if !strings.Contains(string(r.Args), p.ID) || !strings.Contains(string(r.Args), dupID) || strings.Contains(string(r.Args), "chai") {
		t.Errorf("args = %s, want the proposal id and the removed id, never the line", r.Args)
	}
}

func TestRejectingARemovalLeavesTheDocument(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	_, msgID := removal(t, b, tg, doc)
	b.handle(context.Background(), memTap(msgID, "mem-reject"))
	if readDoc(t, doc) != dupDoc {
		t.Fatalf("a rejected removal changed the document:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "❌ rejected") || !strings.Contains(e, "chai") {
		t.Errorf("edit = %q", e)
	}
}

func TestARemovalWhoseLineChangedSinceTheCardIsNotApplied(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	p, msgID := removal(t, b, tg, doc)
	changed := strings.Replace(dupDoc, "chai", "coffee", 1)
	if err := os.WriteFile(doc, []byte(changed), 0o664); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))
	if readDoc(t, doc) != changed {
		t.Fatalf("the document changed:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.HasPrefix(e, "⚠️ not written") {
		t.Errorf("edit = %q", e)
	}
	if !isPending(t, b, p.ID) {
		t.Error("the proposal was resolved")
	}
}

func TestARemovalRetargetedAfterTheCardIsNotApplied(t *testing.T) {
	b, tg, _, _, doc := memBot(t)
	p, msgID := removal(t, b, tg, doc)
	forged := p
	forged.Remove = "20261030-bbbbbbbbbbbbbbbb"
	forged.Line = "likes dosa"
	body, _ := json.Marshal(forged)
	if err := os.WriteFile(b.mem.Root+"/.proposals/"+p.ID+".json", body, 0o644); err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), memTap(msgID, "mem-approve"))
	if readDoc(t, doc) != dupDoc {
		t.Fatalf("a retargeted removal was applied:\n%s", readDoc(t, doc))
	}
	if e, _ := tg.edit(msgID); !strings.Contains(e, "changed after this card was shown") {
		t.Errorf("edit = %q", e)
	}
}
