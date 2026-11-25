package sandboxconfig

import (
	"strings"
	"testing"
)

// A pool writes the newest sandbox.json version the pinned agent declares it
// reads, the legacy one for an agent that declares nothing, and refuses an
// agent that reads neither (ADR 26-11-24-519 §2).
func TestThePoolWritesTheNewestVersionThePinnedAgentReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  map[string]string
		want    string
		refused bool
	}{
		{name: "unlabeled", labels: nil, want: LegacyAPIVersion},
		{name: "other labels only", labels: map[string]string{"org.opencontainers.image.title": "agent"}, want: LegacyAPIVersion},
		{name: "both", labels: map[string]string{AgentReadsLabel: `["discobox.ai/sandbox/v1","discobox.dev/sandbox/v1"]`}, want: APIVersion},
		{name: "new only", labels: map[string]string{AgentReadsLabel: `["discobox.ai/sandbox/v1"]`}, want: APIVersion},
		{name: "legacy only", labels: map[string]string{AgentReadsLabel: `["discobox.dev/sandbox/v1"]`}, want: LegacyAPIVersion},
		{name: "a later version and the new one", labels: map[string]string{AgentReadsLabel: `["discobox.ai/sandbox/v2","discobox.ai/sandbox/v1"]`}, want: APIVersion},
		{name: "neither", labels: map[string]string{AgentReadsLabel: `["discobox.ai/sandbox/v2"]`}, refused: true},
		{name: "empty", labels: map[string]string{AgentReadsLabel: `[]`}, refused: true},
		{name: "set to nothing", labels: map[string]string{AgentReadsLabel: ``}, refused: true},
		{name: "not JSON", labels: map[string]string{AgentReadsLabel: `discobox.ai/sandbox/v1`}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, err := AgentReads(tc.labels)
			var got string
			if err == nil {
				got, err = WriteAPIVersion(reads)
			}
			if tc.refused {
				if err == nil {
					t.Fatalf("wrote %q, want the agent refused", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("wrote %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestARefusedAgentNamesWhatItReadsAndWhatThePoolWrites(t *testing.T) {
	_, err := WriteAPIVersion([]string{"discobox.ai/sandbox/v2"})
	if err == nil || !strings.Contains(err.Error(), "discobox.ai/sandbox/v2") || !strings.Contains(err.Error(), APIVersion) {
		t.Fatalf("err = %v, want it to name what the agent reads and what the pool writes", err)
	}
}
