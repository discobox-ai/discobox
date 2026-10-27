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

// A machine sandbox is never given the pool's BuildKit: a build's steps would
// run on the pool's machine, beside its provisioning key.
func TestMachinePoolEndpointsHaveNoBuildKit(t *testing.T) {
	for _, tunnel := range []string{"", "wss://pool.example.com"} {
		endpoints := machinePoolEndpoints(tunnel)
		if endpoints.BuildKit != "" || endpoints.Proxy == "" {
			t.Errorf("machinePoolEndpoints(%q) = %+v, want the proxy and no BuildKit", tunnel, endpoints)
		}
	}
}

// A machine pool's mediator serves no builds, whatever a sandbox is told:
// every sandbox of it holds a certificate the mediator would accept. A Docker
// pool's serves them, and so does a box with no bootstrap file.
func TestMediatorServesNoMachinePool(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		bootstrap string
		want      bool
	}{
		{`{"controlPlaneUrl":"http://cp","poolId":"pool_1","runtime":"machine","driver":"boxd","machineProject":"pool-1"}`, false},
		{`{"controlPlaneUrl":"http://cp","poolId":"pool_1"}`, true},
		{`{"controlPlaneUrl":"http://cp","poolId":"pool_1","runtime":"docker"}`, true},
		{"", true},
	} {
		path := filepath.Join(dir, "absent.json")
		if tc.bootstrap != "" {
			path = filepath.Join(dir, "bootstrap.json")
			if err := os.WriteFile(path, []byte(tc.bootstrap), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := mediatorServes(path); err != nil || got != tc.want {
			t.Errorf("mediatorServes(%s) = %v, %v, want %v", tc.bootstrap, got, err, tc.want)
		}
	}
}
