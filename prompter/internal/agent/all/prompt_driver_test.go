package all

import (
	"testing"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

func TestPromptDriversBuildProviderCommands(t *testing.T) {
	tests := []struct {
		name              string
		kind              agent.Kind
		request           agent.RunRequest
		providerSessionID string
		want              agent.PromptCommand
	}{
		{
			name: "claude direct uuid",
			kind: agent.KindClaudeCode,
			request: agent.RunRequest{
				SessionID: "11111111-1111-1111-1111-111111111111",
				Prompt:    "do work",
				Workdir:   "/workspace/project",
			},
			want: agent.PromptCommand{
				Command:         agent.Command{Name: "claude", Args: []string{"--bare", "--permission-mode", "bypassPermissions", "-p", "do work", "--output-format", "json", "--session-id", "11111111-1111-1111-1111-111111111111"}, Dir: "/workspace/project"},
				DirectSessionID: true,
			},
		},
		{
			name: "codex normalized options",
			kind: agent.KindCodex,
			request: agent.RunRequest{
				Prompt:      "do work",
				Model:       "gpt-5.5",
				Reasoning:   "high",
				ServiceTier: "flex",
				Workdir:     "/workspace/project",
			},
			want: agent.PromptCommand{Command: agent.Command{Name: "codex", Args: []string{"exec", "--json", "--model", "gpt-5.5", "--config", "model_reasoning_effort=\"high\"", "--config", "model_service_tier=\"flex\"", "--ephemeral", "do work"}, Dir: "/workspace/project"}},
		},
		{
			name:              "gemini resume",
			kind:              agent.KindGeminiCLI,
			providerSessionID: "provider-1",
			request: agent.RunRequest{
				Prompt:  "continue work",
				Model:   "gemini-3.5-flash",
				Workdir: "/workspace/project",
			},
			want: agent.PromptCommand{Command: agent.Command{Name: "gemini", Args: []string{"--prompt", "continue work", "--output-format", "json", "--skip-trust", "--model", "gemini-3.5-flash", "--resume", "provider-1"}, Dir: "/workspace/project"}},
		},
		{
			name:              "opencode resume",
			kind:              agent.KindOpenCode,
			providerSessionID: "provider-1",
			request: agent.RunRequest{
				Prompt:    "continue work",
				Agent:     "build",
				Model:     "openai/gpt-5.5",
				Reasoning: "high",
				Workdir:   "/workspace/project",
			},
			want: agent.PromptCommand{Command: agent.Command{Name: "opencode", Args: []string{"run", "--format", "json", "--dir", "/workspace/project", "--model", "openai/gpt-5.5", "--agent", "build", "--variant", "high", "--session", "provider-1", "continue work"}, Dir: "/workspace/project"}},
		},
		{
			name:              "discobot json prompt",
			kind:              agent.KindDiscobot,
			providerSessionID: "thread-1",
			request: agent.RunRequest{
				Prompt:      "do work",
				Agent:       "reviewer",
				Model:       "openai/gpt-5.5",
				Reasoning:   "high",
				ServiceTier: "flex",
				Workdir:     "/workspace/project",
			},
			want: agent.PromptCommand{Command: agent.Command{Name: "disco", Args: []string{"--print", "--json", "--model", "openai/gpt-5.5", "--subagent", "reviewer", "--reasoning", "high", "--service-tier", "flex", "--resume", "thread-1", "do work"}, Dir: "/workspace/project"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, ok := agent.PromptDriverFor(test.kind)
			if !ok {
				t.Fatalf("missing prompt driver for %s", test.kind)
			}
			got := driver.Command(test.request, test.providerSessionID)
			if got.DirectSessionID != test.want.DirectSessionID || got.Command.Name != test.want.Command.Name || got.Command.Dir != test.want.Command.Dir || !equalStrings(got.Command.Args, test.want.Command.Args) {
				t.Fatalf("expected %#v, got %#v", test.want, got)
			}
		})
	}
}

func TestRunnerForUsesRegisteredPromptDrivers(t *testing.T) {
	for _, kind := range []agent.Kind{agent.KindClaudeCode, agent.KindCodex, agent.KindGeminiCLI, agent.KindOpenCode, agent.KindDiscobot} {
		if _, ok := agent.RunnerFor(agent.Detected{Kind: kind}); !ok {
			t.Fatalf("expected runner for %s", kind)
		}
	}
	if _, ok := agent.RunnerFor(agent.Detected{Kind: agent.KindUnknown}); ok {
		t.Fatal("expected no runner for unknown kind")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
