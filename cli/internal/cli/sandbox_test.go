package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCreateSandboxBodyIncludesAgentLaunchFields(t *testing.T) {
	t.Setenv("SANDBOX_ENV_FROM_SHELL", "from-shell")
	body, err := createSandboxBody(sandboxCreateOptions{
		name:                     "work",
		agentName:                "Codex",
		agentModel:               "gpt-5.1-codex-max",
		agentModelServiceTier:    "priority",
		agentModelReasoningLevel: "high",
		prompt:                   []string{"implement this"},
		env:                      []string{"EXPLICIT=value", "SANDBOX_ENV_FROM_SHELL"},
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
	if body.AgentName.Value != "Codex" || body.Config.AgentModel.Value != "gpt-5.1-codex-max" ||
		len(body.Config.Prompt) != 1 || body.Config.Prompt[0] != "implement this" {
		t.Fatalf("agent fields = %#v", body)
	}
	env, ok := body.Config.Env.Get()
	if !ok {
		t.Fatal("expected env")
	}
	if env["EXPLICIT"] != "value" || env["SANDBOX_ENV_FROM_SHELL"] != "from-shell" {
		t.Fatalf("env = %#v, want explicit and shell values", env)
	}
	source, ok := body.Config.Source.Get()
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
	sandboxUser, ok := body.Config.User.Get()
	if !ok {
		t.Fatal("expected user")
	}
	if sandboxUser.Name.Value != "darren" || sandboxUser.UID.Value != 1000 || sandboxUser.Gid.Value != 1000 || sandboxUser.HomeDirectory.Value != "/home/darren" {
		t.Fatalf("user = %s %d/%d home %s, want darren 1000/1000 /home/darren", sandboxUser.Name.Value, sandboxUser.UID.Value, sandboxUser.Gid.Value, sandboxUser.HomeDirectory.Value)
	}
	ref, ok := body.Config.SourceCodeReferences.Value["lib"]
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

	// A plain q after Ctrl-P is passed through, not a detach: the docker-style
	// sequence requires Ctrl-Q (0x11), and Ctrl-P is a common editing key.
	out, detach = filterDetachSequence([]byte{0x10, 'q'}, &pending)
	if detach || string(out) != string([]byte{0x10, 'q'}) || pending {
		t.Fatalf("ctrl-p then plain q = %v detach=%t pending=%t", out, detach, pending)
	}

	out, detach = filterDetachSequence([]byte{0x10, 0x11}, &pending)
	if !detach || len(out) != 0 || pending {
		t.Fatalf("detach sequence = %v detach=%t pending=%t", out, detach, pending)
	}
}
