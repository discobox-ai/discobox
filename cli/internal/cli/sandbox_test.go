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
		homeDirectory:            "/home/darren",
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
	sandboxUser, ok := body.User.Get()
	if !ok {
		t.Fatal("expected user")
	}
	if sandboxUser.Name.Value != "darren" || sandboxUser.UID.Value != 1000 || sandboxUser.Gid.Value != 1000 || sandboxUser.HomeDirectory.Value != "/home/darren" {
		t.Fatalf("user = %s %d/%d home %s, want darren 1000/1000 /home/darren", sandboxUser.Name.Value, sandboxUser.UID.Value, sandboxUser.Gid.Value, sandboxUser.HomeDirectory.Value)
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

func TestTerminalDetachSequenceFilter(t *testing.T) {
	pending := false
	out, detach := filterDetachSequence([]byte("abc"), &pending)
	if detach || string(out) != "abc" || pending {
		t.Fatalf("plain input = %q detach=%t pending=%t", out, detach, pending)
	}

	out, detach = filterDetachSequence([]byte{0x10}, &pending)
	if detach || string(out) != "" || !pending {
		t.Fatalf("ctrl-p input = %q detach=%t pending=%t", out, detach, pending)
	}

	out, detach = filterDetachSequence([]byte("x"), &pending)
	if detach || string(out) != string([]byte{0x10, 'x'}) || pending {
		t.Fatalf("non-detach followup = %v detach=%t pending=%t", out, detach, pending)
	}

	out, detach = filterDetachSequence([]byte{0x10, 'q'}, &pending)
	if !detach || len(out) != 0 || pending {
		t.Fatalf("detach sequence = %v detach=%t pending=%t", out, detach, pending)
	}
}
