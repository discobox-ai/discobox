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
		sourceCodeReferences:     `{"lib":{"kind":"git","url":"https://example.com/lib.git","checkout":{"commit":"abc123","refType":"commit"},"destination":{"directory":"/workspace/lib"}}}`,
		userName:                 "darren",
		userUID:                  1000,
		userGID:                  1000,
	})
	if err != nil {
		t.Fatalf("createSandboxBody: %v", err)
	}
	if body.AgentName.Value != "Codex" || body.AgentModel.Value != "gpt-5.1-codex-max" || body.Prompt.Value != "implement this" {
		t.Fatalf("agent fields = %#v", body)
	}
	source, ok := body.Source.Get()
	if !ok {
		t.Fatal("expected source")
	}
	if source.Kind != "git" {
		t.Fatalf("source kind = %q, want git", source.Kind)
	}
	destination, ok := source.Destination.Get()
	if !ok {
		t.Fatal("expected source destination")
	}
	if destination.Directory.Value != "/workspace/repo" || destination.WorkingDirectory.Value != "/workspace/repo" {
		t.Fatalf("directories = source %q working %q", destination.Directory.Value, destination.WorkingDirectory.Value)
	}
	if body.UserName.Value != "darren" || body.UserUid.Value != 1000 || body.UserGid.Value != 1000 {
		t.Fatalf("user = %s %d/%d, want darren 1000/1000", body.UserName.Value, body.UserUid.Value, body.UserGid.Value)
	}
	ref, ok := body.SourceCodeReferences.Value["lib"]
	if !ok {
		t.Fatal("expected lib source reference")
	}
	refDestination, ok := ref.Destination.Get()
	if !ok {
		t.Fatal("expected lib destination")
	}
	if refDestination.Directory.Value != "/workspace/lib" {
		t.Fatalf("ref directory = %q, want /workspace/lib", refDestination.Directory.Value)
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
