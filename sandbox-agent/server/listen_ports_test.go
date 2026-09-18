package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A port Discobox's own plumbing listens on is never a discobox's to forward,
// and a forwarder's must never be probed: a probe would be carried out of the
// sandbox. The forwarders' ports are read from the bridge configs the pool
// stages, and a config that is absent names nothing.
func TestAgentListenPortsCoverEveryAgentListener(t *testing.T) {
	dir := t.TempDir()
	egress := filepath.Join(dir, "bridge.json")
	buildkit := filepath.Join(dir, "bridge-buildkit.json")
	if err := os.WriteFile(egress, []byte(`{"listenAddress":"127.0.0.1:17008","workerProxyUrl":"https://pool:17080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildkit, []byte(`{"listenAddress":"127.0.0.1:17082"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := agentListenPorts("0.0.0.0:3002", egress, buildkit, filepath.Join(dir, "absent.json"))
	for _, want := range []int{3002, 17010, 17008, 17082} {
		if !slices.Contains(got, want) {
			t.Fatalf("agentListenPorts = %v, missing %d", got, want)
		}
	}
	if len(got) != 4 {
		t.Fatalf("agentListenPorts = %v, want exactly the four listeners", got)
	}
}
