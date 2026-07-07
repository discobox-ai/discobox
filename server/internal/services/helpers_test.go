package services

import (
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

// A sandbox's embedded agent config is stored sparse (definition-backed fields
// unset). The response must resolve it so runCommand — required by the schema —
// is present; otherwise clients fail to decode the sandbox.
func TestSandboxToAPIResolvesEmbeddedAgentConfig(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:        "sb_1",
		ProjectID: "p1",
		AgentConfig: &model.AgentConfig{
			ID:           "ac_1",
			ProjectID:    "p1",
			Slug:         "codex",
			DefinitionID: "codex",
			Name:         "Codex",
			// RunCommand/RelaunchCommand intentionally unset (sparse).
		},
	}
	out, err := SandboxToAPI(sandbox)
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	agentConfig, ok := out.AgentConfig.Get()
	if !ok {
		t.Fatalf("expected embedded agent config")
	}
	if got := agentConfig.RunCommand; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("runCommand = %#v, want [codex] resolved from the definition", got)
	}
}
