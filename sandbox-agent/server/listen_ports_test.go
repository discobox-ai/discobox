package server

import (
	"slices"
	"testing"
)

// A port Discobox's own plumbing listens on is never a discobox's to forward,
// and a forwarder's must never be probed: a probe would be carried out of the
// sandbox. The forwarders' addresses are the sandbox's own, so they are
// excluded before any proxy material has been delivered to start them
// (ADR 26-10-08-127 §4). The desktop's stream is the desktop viewer's, reached
// only through its own port (ADR 26-11-06-851 §1).
func TestAgentListenPortsCoverEveryAgentListener(t *testing.T) {
	got := agentListenPorts("0.0.0.0:3002")
	for _, want := range []int{3002, 17010, 17008, 53, 17082, 17063} {
		if !slices.Contains(got, want) {
			t.Fatalf("agentListenPorts = %v, missing %d", got, want)
		}
	}
	if len(got) != 6 {
		t.Fatalf("agentListenPorts = %v, want exactly the six listeners", got)
	}
}
