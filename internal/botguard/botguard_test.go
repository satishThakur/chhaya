package botguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTG struct {
	id       int64
	username string
	err      error
}

func (f fakeTG) GetMe(context.Context) (int64, string, error) { return f.id, f.username, f.err }

func bots(h, a string) (Telegram, Telegram) {
	return fakeTG{id: 1, username: h}, fakeTG{id: 2, username: a}
}

func verify(t *testing.T, hermesEnv, gatewayEnv, h, a string) error {
	t.Helper()
	ht, at := bots(h, a)
	return Verify(context.Background(), hermesEnv, gatewayEnv, ht, at)
}

// --- the pairs that must start ---------------------------------------------

func TestDevPairOnDevStarts(t *testing.T) {
	if err := verify(t, "dev", "dev", "chhaya_dev_bot", "chhaya_dev_approver_bot"); err != nil {
		t.Fatalf("the dev pair in a dev environment must start: %v", err)
	}
}

func TestProdPairOnProdStarts(t *testing.T) {
	if err := verify(t, "prod", "prod", "chhaya_bot", "chhaya_approver_bot"); err != nil {
		t.Fatalf("the prod pair in a prod environment must start: %v", err)
	}
}

func TestUsernamesAreComparedCaseInsensitively(t *testing.T) {
	if err := verify(t, "dev", "dev", "Chhaya_DEV_Bot", "Chhaya_Dev_Approver_Bot"); err != nil {
		t.Fatalf("Telegram usernames are case-insensitive: %v", err)
	}
}

// --- every refusal ----------------------------------------------------------

func TestProdBotsRefusedInDevEnvironment(t *testing.T) {
	// The mistake this package exists to stop: the desktop starting with the
	// production tokens and polling the same bots as the box.
	err := verify(t, "dev", "dev", "chhaya_bot", "chhaya_approver_bot")
	if err == nil {
		t.Fatal("production bots must be refused when CHHAYA_ENV=dev")
	}
	for _, want := range []string{"hermes", "approver"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error should name both bots, missing %q: %v", want, err)
		}
	}
}

func TestDevBotsRefusedInProdEnvironment(t *testing.T) {
	if err := verify(t, "prod", "prod", "chhaya_dev_bot", "chhaya_dev_approver_bot"); err == nil {
		t.Fatal("dev bots must be refused when CHHAYA_ENV=prod")
	}
}

func TestOnlyOneProdTokenInDevIsStillRefused(t *testing.T) {
	if err := verify(t, "dev", "dev", "chhaya_bot", "chhaya_dev_approver_bot"); err == nil {
		t.Fatal("a prod Hermes bot with a dev approver must be refused in dev")
	}
	if err := verify(t, "dev", "dev", "chhaya_dev_bot", "chhaya_approver_bot"); err == nil {
		t.Fatal("a dev Hermes bot with a prod approver must be refused in dev")
	}
}

func TestSwappedTokensAreRefused(t *testing.T) {
	// Approver token in the Hermes file would let the agent forge approvals.
	if err := verify(t, "prod", "prod", "chhaya_approver_bot", "chhaya_bot"); err == nil {
		t.Fatal("swapped tokens must be refused in prod")
	}
	if err := verify(t, "dev", "dev", "chhaya_dev_approver_bot", "chhaya_dev_bot"); err == nil {
		t.Fatal("swapped tokens must be refused in dev")
	}
}

func TestOneTokenInBothFilesIsRefused(t *testing.T) {
	same := fakeTG{id: 7, username: "chhaya_approver_bot"}
	if err := Verify(context.Background(), "prod", "prod", same, same); err == nil {
		t.Fatal("the same bot as both Hermes' and the approver must be refused")
	}
}

func TestUnreachableTelegramFailsClosed(t *testing.T) {
	down := fakeTG{err: errors.New("telegram getMe: dial tcp: no route to host")}
	ok := fakeTG{id: 2, username: "chhaya_dev_approver_bot"}
	if err := Verify(context.Background(), "dev", "dev", down, ok); err == nil {
		t.Fatal("an unreachable getMe on Hermes' token must refuse")
	}
	okh := fakeTG{id: 1, username: "chhaya_dev_bot"}
	if err := Verify(context.Background(), "dev", "dev", okh, down); err == nil {
		t.Fatal("an unreachable getMe on the approver token must refuse")
	}
}

func TestMissingEnvironmentIsRefused(t *testing.T) {
	if err := verify(t, "", "", "chhaya_dev_bot", "chhaya_dev_approver_bot"); err == nil {
		t.Fatal("a missing CHHAYA_ENV must refuse; there is no default")
	}
	if err := verify(t, "dev", "", "chhaya_dev_bot", "chhaya_dev_approver_bot"); err == nil {
		t.Fatal("CHHAYA_ENV missing from the gateway file must refuse")
	}
	if err := verify(t, "", "dev", "chhaya_dev_bot", "chhaya_dev_approver_bot"); err == nil {
		t.Fatal("CHHAYA_ENV missing from the Hermes file must refuse")
	}
}

func TestUnrecognisedEnvironmentIsRefused(t *testing.T) {
	for _, v := range []string{"staging", "Prod ", "production", "DEV", "true"} {
		if err := verify(t, v, v, "chhaya_bot", "chhaya_approver_bot"); err == nil {
			t.Errorf("CHHAYA_ENV=%q must be refused: only dev and prod exist", v)
		}
	}
}

func TestTheTwoFilesMustAgreeOnTheEnvironment(t *testing.T) {
	if err := verify(t, "dev", "prod", "chhaya_dev_bot", "chhaya_approver_bot"); err == nil {
		t.Fatal("dev in one file and prod in the other must be refused")
	}
}

func TestBotWithNoUsernameIsRefused(t *testing.T) {
	if err := verify(t, "prod", "prod", "", "chhaya_approver_bot"); err == nil {
		t.Fatal("a bot reporting no username cannot be classified and must be refused")
	}
}

func TestErrorsNeverContainATokenShapedString(t *testing.T) {
	// Tokens never reach this package, but the point stands for what we return.
	err := verify(t, "dev", "dev", "chhaya_bot", "chhaya_approver_bot")
	if err == nil || strings.Contains(err.Error(), ":AA") {
		t.Fatalf("unexpected: %v", err)
	}
}

// --- reading the env files --------------------------------------------------

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadEnvFileParsesKeysCommentsAndQuotes(t *testing.T) {
	p := writeEnv(t, "# comment\n\nCHHAYA_ENV=dev\nTELEGRAM_BOT_TOKEN=\"123:abc\"\nEMPTY=\nX='y'\n  # indented comment\n")
	m, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["CHHAYA_ENV"] != "dev" || m["TELEGRAM_BOT_TOKEN"] != "123:abc" || m["X"] != "y" {
		t.Errorf("parsed wrongly: %v", m)
	}
	if v, ok := m["EMPTY"]; !ok || v != "" {
		t.Errorf("an empty value is present and empty: %v", m)
	}
}

func TestReadEnvFileMissingFails(t *testing.T) {
	if _, err := ReadEnvFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing env file must be an error")
	}
}
