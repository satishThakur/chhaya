package health

import (
	"context"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestNoDatabaseURLMeansTheModuleIsOff(t *testing.T) {
	rt, err := FromEnv(context.Background(), env(map[string]string{}), t.TempDir(), &fakeAudit{}, func() bool { return false })
	if err != nil || rt != nil {
		t.Fatalf("%v %v", rt, err)
	}
}

func TestADatabaseURLWithoutAGoodSecretRefusesToStart(t *testing.T) {
	for _, secret := range []string{"", "short"} {
		_, err := FromEnv(context.Background(), env(map[string]string{"CHHAYA_HEALTH_DB_URL": "postgres://x", "CHHAYA_HEALTH_INGEST_SECRET": secret}), t.TempDir(), &fakeAudit{}, func() bool { return false })
		if err == nil || !strings.Contains(err.Error(), "CHHAYA_HEALTH_INGEST_SECRET") {
			t.Fatalf("secret %q: %v", secret, err)
		}
	}
}

func TestABadTimeZoneRefusesToStart(t *testing.T) {
	_, err := FromEnv(context.Background(), env(map[string]string{
		"CHHAYA_HEALTH_DB_URL": "postgres://x", "CHHAYA_HEALTH_INGEST_SECRET": "0123456789abcdef0123", "CHHAYA_HEALTH_TZ": "Mars/Olympus"}),
		t.TempDir(), &fakeAudit{}, func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "CHHAYA_HEALTH_TZ") {
		t.Fatalf("%v", err)
	}
}

func TestAnUnreachableDatabaseIsAnErrorAtBoot(t *testing.T) {
	_, err := FromEnv(context.Background(), env(map[string]string{
		"CHHAYA_HEALTH_DB_URL": "postgres://u:p@127.0.0.1:1/x?connect_timeout=1", "CHHAYA_HEALTH_INGEST_SECRET": "0123456789abcdef0123", "CHHAYA_HEALTH_DB_WAIT_SECONDS": "0"}),
		t.TempDir(), &fakeAudit{}, func() bool { return false })
	if err == nil {
		t.Fatal("a gateway that cannot reach its health store must say so at boot")
	}
}
