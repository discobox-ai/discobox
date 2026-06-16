package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCreateSandboxBodyIncludesAgentLaunchFields(t *testing.T) {
	body, err := createSandboxBody(sandboxCreateOptions{
		name:                     "work",
		agentName:                "Codex",
		agentModel:               "gpt-5.1-codex-max",
		agentModelServiceTier:    "priority",
		agentModelReasoningLevel: "high",
		prompt:                   "implement this",
		sourceURL:                "https://example.com/repo.git",
		sourceRef:                "main",
		sourceRefType:            "branch",
		sourceDirectory:          "/workspace/repo",
		workingDirectory:         "/workspace/repo",
		sourceCodeReferences:     `{"lib":{"url":"https://example.com/lib.git","ref":"abc123","refType":"commit","directory":"/workspace/lib"}}`,
		userUID:                  1000,
		userGID:                  1000,
	})
	if err != nil {
		t.Fatalf("createSandboxBody: %v", err)
	}
	if body.AgentName.Value != "Codex" || body.AgentModel.Value != "gpt-5.1-codex-max" || body.Prompt.Value != "implement this" {
		t.Fatalf("agent fields = %#v", body)
	}
	if body.SourceDirectory.Value != "/workspace/repo" || body.WorkingDirectory.Value != "/workspace/repo" {
		t.Fatalf("directories = source %q working %q", body.SourceDirectory.Value, body.WorkingDirectory.Value)
	}
	if body.UserUid.Value != 1000 || body.UserGid.Value != 1000 {
		t.Fatalf("uid/gid = %d/%d, want 1000/1000", body.UserUid.Value, body.UserGid.Value)
	}
	ref, ok := body.SourceCodeReferences.Value["lib"]
	if !ok {
		t.Fatal("expected lib source reference")
	}
	if ref.Directory != "/workspace/lib" {
		t.Fatalf("ref directory = %q, want /workspace/lib", ref.Directory)
	}
}

func TestUpdateSandboxBodyIsNameOnly(t *testing.T) {
	cmd := &cobra.Command{}
	addUpdateFlags(cmd, &sandboxUpdateOptions{})
	if cmd.Flags().Lookup("description") != nil {
		t.Fatal("sandbox update should not expose description flag")
	}
	if cmd.Flags().Lookup("source-url") != nil {
		t.Fatal("sandbox update should not expose source-url flag")
	}
	if cmd.Flags().Lookup("name") == nil {
		t.Fatal("sandbox update should expose name flag")
	}
}
