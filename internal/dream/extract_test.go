package dream

import (
	"strings"
	"testing"
)

func TestConversationLabelsWhoSaidWhat(t *testing.T) {
	all := renderConversation(in().Trace)
	for _, want := range []string{"[1] OWNER:", "[2] ASSISTANT:", "[3] TOOL OUTPUT", "[4] OWNER: I already heard Foo and Bar", "[5] SYSTEM NOTE"} {
		if !strings.Contains(all, want) {
			t.Errorf("conversation lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "old text") {
		t.Error("an inactive message must not be shown")
	}
	if strings.Contains(all, "[5] OWNER") || strings.Contains(all, "[3] OWNER") {
		t.Error("tool output or a system note was labelled as the owner")
	}
}
