package services

import (
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

// A sandbox's embedded harness config is stored sparse (definition-backed fields
// unset). The response must resolve it so runCommand — required by the schema —
// is present; otherwise clients fail to decode the sandbox.
func TestSandboxToAPIResolvesEmbeddedHarnessConfig(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:        "sb_1",
		ProjectID: "p1",
		HarnessConfig: &model.HarnessConfig{
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
	harnessConfig, ok := out.HarnessConfig.Get()
	if !ok {
		t.Fatalf("expected embedded harness config")
	}
	if got := harnessConfig.RunCommand; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("runCommand = %#v, want [codex] resolved from the definition", got)
	}
}
