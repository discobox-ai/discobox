package services

import (
	"testing"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
	"github.com/obot-platform/discobox/server/internal/model"
)

// The list-harness-definitions handler serves the built-in definitions through
// Convert, which round-trips them through the generated API type's decoder and
// its required-field validation. Every built-in definition — including its
// optional configure block — must survive that decode, or the endpoint 500s.
// This guards against the model and OpenAPI schema drifting apart.
func TestBuiltInHarnessDefinitionsConvertToAPI(t *testing.T) {
	definitions := harnessdefs.Definitions()
	if len(definitions) == 0 {
		t.Fatal("no built-in harness definitions to check")
	}
	if _, err := Convert[apimodel.ListHarnessDefinitionsBody](struct {
		HarnessDefinitions any `json:"harnessDefinitions"`
	}{HarnessDefinitions: definitions}); err != nil {
		t.Fatalf("convert built-in harness definitions to API: %v", err)
	}
}

func TestSandboxToAPIIncludesRegisteredHarnessConfig(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:        "sb_1",
		ProjectID: "p1",
		HarnessConfig: &model.HarnessConfig{
			ID:           "ac_1",
			ProjectID:    "p1",
			Slug:         "codex",
			DefinitionID: "codex",
			Name:         "Codex",
			RunCommand:   []string{"codex"},
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
		t.Fatalf("runCommand = %#v, want registered [codex]", got)
	}
}
