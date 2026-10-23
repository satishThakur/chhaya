package mcpsrv

import (
	"os"
	"strings"
	"testing"
)

func TestCheckEnvironmentIsolation(t *testing.T) {
	const approver = "8940896193:AA-approver"

	tests := []struct {
		name     string
		env      map[string]string
		wantErr  bool
		mentions string
	}{
		{
			name: "the normal case",
			env:  map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver},
		},
		{
			name:     "Hermes' token is visible here",
			env:      map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": "8979753688:AA-hermes"},
			wantErr:  true,
			mentions: "TELEGRAM_BOT_TOKEN",
		},
		{
			name:    "Hermes' token is present but empty — still the wrong env file",
			env:     map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": ""},
			wantErr: false, // an empty value is indistinguishable from unset via Getenv
		},
		{
			name:    "no approver token at all",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "the two tokens are the same",
			env:     map[string]string{"CHHAYA_APPROVER_BOT_TOKEN": approver, "TELEGRAM_BOT_TOKEN": approver},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckEnvironmentIsolation(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.mentions != "" && err != nil && !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error should name the variable: %v", err)
			}
		})
	}
}

// MCPHandler reads the registry to publish schemas, but must reach handlers
// only through Invoke. Assert that its body never calls reg.Handle.
func TestMCPHandlerDoesNotCallHandlersDirectly(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), ".Handle(") {
		t.Fatal("server.go calls a handler directly; it must go through Invoke")
	}
}

// TestGatewayNeverSamplesOrElicits is the structural half of spec §3.4.
// Elicitation would surface approval prompts through the agent, defeating the
// separate approver bot; sampling would let a tool borrow the model mid-call.
// Both are disabled in Hermes' config, and both default to on — so this test
// is what stops us relying on a config file we do not own.
func TestGatewayNeverSamplesOrElicits(t *testing.T) {
	forbidden := []string{"CreateMessage(", "CreateMessageWithTools(", "Elicit(", "NotifyElicitationComplete("}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s calls %s; the gateway must never ask the model or the user anything "+
					"through the MCP session — approvals go to the approver bot", e.Name(), f)
			}
		}
	}
}
