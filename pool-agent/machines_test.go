package poolagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provisioning key is read on every call, so a key the provider renews is
// used from the next call with no new engine; and nothing of the file reaches
// an error (ADR 26-10-23-551 §4).
func TestProvisioningKeysReadTheFileOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provisioning-key.json")
	keys := provisioningKeys(path, "boxd")
	write := func(key string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := keys(context.Background()); err == nil || !strings.Contains(err.Error(), "no provisioning key") {
		t.Fatalf("no file = %v, want it said", err)
	}
	write(`{"driver":"boxd","secret":"first-secret","id":"key-1","issued":true}`)
	if key, err := keys(context.Background()); err != nil || key.Secret != "first-secret" {
		t.Fatalf("key = %v, %v; want the first", key, err)
	}
	write(`{"driver":"boxd","secret":"renewed-secret","id":"key-2","issued":true}`)
	if key, err := keys(context.Background()); err != nil || key.Secret != "renewed-secret" {
		t.Fatalf("key = %v, %v; want the renewed one", key, err)
	}
	write(`{"driver":"docker","secret":"other-secret"}`)
	if _, err := keys(context.Background()); err == nil || strings.Contains(err.Error(), "other-secret") {
		t.Fatalf("another driver's key = %v, want refused without its secret", err)
	}
	write(`{"driver":"boxd","secret":"broken-secret`)
	if _, err := keys(context.Background()); err == nil || strings.Contains(err.Error(), "broken-secret") {
		t.Fatalf("a file that does not decode = %v, want refused without its contents", err)
	}
}
