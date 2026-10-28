package poolagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
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

// A host is given the pool's server name in its hosts file, once, whatever
// DNS answers for it: a resolver must not choose where builds are pushed. A
// line that names it already is left alone.
func TestEnsurePoolNameIsTheHostsFiles(t *testing.T) {
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n# 10.0.0.9 "+proxyagent.ServerName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePoolName(hosts); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(hosts)
	if !strings.Contains(string(data), "127.0.0.1 localhost\n") || !strings.Contains(string(data), "\n127.0.0.1 "+proxyagent.ServerName+"\n") {
		t.Fatalf("hosts = %q, want the pool's name beside what was there; a commented line names nothing", data)
	}
	if err := ensurePoolName(hosts); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(hosts); string(again) != string(data) {
		t.Fatalf("the name was added again: %q", again)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if err := ensurePoolName(missing); err != nil {
		t.Fatalf("a host with no hosts file: %v", err)
	}
}
