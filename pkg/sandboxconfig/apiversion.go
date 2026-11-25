package sandboxconfig

import (
	"encoding/json"
	"fmt"
	"slices"
)

// APIVersion is the sandbox.json contract version, in Discobox's own
// namespace.
const APIVersion = "discobox.ai/sandbox/v1"

// LegacyAPIVersion is the same contract under the namespace it was first
// written in, which Discobox does not own. A pool writes it for an agent that
// declares nothing it reads (WriteAPIVersion), and a sandbox agent keeps
// reading it: a Docker box keeps the file it was built with across in-place
// pin moves, and a pool from before the agent declared what it reads writes
// nothing else (ADR 26-11-24-519 §4).
const LegacyAPIVersion = "discobox.dev/sandbox/v1"

// ReadAPIVersions is every sandbox.json apiVersion a sandbox agent built from
// this tree reads, newest first. The agent bundle declares it in
// AgentReadsLabel, which sandbox-agent/Dockerfile sets and a test holds equal
// to this.
var ReadAPIVersions = []string{APIVersion, LegacyAPIVersion}

// AgentReadsLabel is the agent bundle's image config label declaring which
// sandbox.json apiVersion values its agent reads, as a JSON array (ADR
// 26-11-24-519 §1).
const AgentReadsLabel = "io.discobox.agent.v1.sandbox-config"

// AgentReads is the sandbox.json apiVersion values an agent bundle declares
// it reads, from its labels. A bundle without AgentReadsLabel was built
// before the label, and its agent reads LegacyAPIVersion alone, as every
// agent from before APIVersion does.
func AgentReads(labels map[string]string) ([]string, error) {
	value, ok := labels[AgentReadsLabel]
	if !ok {
		return []string{LegacyAPIVersion}, nil
	}
	var reads []string
	if err := json.Unmarshal([]byte(value), &reads); err != nil {
		return nil, fmt.Errorf("the agent bundle's %s label %q: %w", AgentReadsLabel, value, err)
	}
	return reads, nil
}

// WriteAPIVersion is the sandbox.json apiVersion a pool writes for an agent
// that reads reads: APIVersion where it reads that, and LegacyAPIVersion
// where it reads only that. An agent that reads neither is refused rather
// than sent a value it would refuse at boot (ADR 26-11-24-519 §2).
func WriteAPIVersion(reads []string) (string, error) {
	for _, version := range ReadAPIVersions {
		if slices.Contains(reads, version) {
			return version, nil
		}
	}
	return "", fmt.Errorf("the agent reads sandbox.json apiVersion %q, and this pool writes %q", reads, ReadAPIVersions)
}
