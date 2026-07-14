package claudecode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/obot-platform/discobox/harness"
)

const ManagedSettingsPath = "/etc/claude-code/managed-settings.json"

// configureScript runs as the primary terminal of the claude-code Configure
// sandbox. See configure.sh for what it does.
//
//go:embed configure.sh
var configureScript string

// configureScriptPath is where configureScript is written in the configure
// sandbox's home directory (see Definition().Configure.Files) and the path
// its RunCommand executes.
const configureScriptPath = ".discobox-configure.sh"

var Events = []string{
	"SessionStart", "Setup", "InstructionsLoaded", "UserPromptSubmit",
	"UserPromptExpansion", "MessageDisplay", "PreToolUse", "PermissionRequest",
	"PostToolUse", "PostToolUseFailure", "PostToolBatch", "PermissionDenied",
	"Notification", "SubagentStart", "SubagentStop", "TaskCreated",
	"TaskCompleted", "Stop", "StopFailure", "TeammateIdle", "ConfigChange",
	"CwdChanged", "FileChanged", "WorktreeCreate", "WorktreeRemove",
	"PreCompact", "PostCompact", "SessionEnd", "Elicitation",
	"ElicitationResult",
}

type Driver struct{}

func (Driver) ID() string { return "claude-code" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID:              "claude-code",
		Name:            "Claude Code",
		Description:     "Anthropic Claude Code coding harness.",
		InstallCommand:  []string{"npm", "install", "-g", "@anthropic-ai/claude-code"},
		RunCommand:      []string{"claude"},
		RelaunchCommand: []string{"claude", "--continue"},
		Files: []harness.File{
			{
				Path:       ".claude.json",
				CreateOnly: true,
				Template:   true,
				Content: `{
  "hasCompletedOnboarding": true
  {{- with .source }},
  "projects": {
    {{ .destination.directory | json }}: {
      "hasTrustDialogAccepted": true
    }
  }
  {{- end }}
}`,
			},
			{Path: ".claude/settings.json", Content: `{"theme":"dark","skipDangerousModePermissionPrompt":true}`},
		},
		Secrets: []harness.Secret{
			{Name: "ANTHROPIC_API_KEY", Required: true, OneOfGroup: "auth"},
			{Name: "CLAUDE_CODE_OAUTH_TOKEN", Required: true, OneOfGroup: "auth"},
		},
		Configure: &harness.Configure{
			InstallCommand: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"},
			RunCommand:     []string{"sh", configureScriptPath},
			Files: []harness.File{
				{Path: configureScriptPath, Content: configureScript},
			},
		},
	}
}

func (Driver) InstallHooks(_ context.Context, req harness.HookInstallRequest) error {
	path := harness.ManagedPath(req.ManagedRoot, ManagedSettingsPath)
	publisher := harness.PublisherCommand(req)
	return harness.MergeJSONFile(path, func(settings map[string]any) {
		hooks := harness.SetJSONMap(settings, "hooks")
		for _, event := range Events {
			harness.UpsertEventCommandHook(hooks, event, "*", commandHook(publisher, event))
		}
	})
}

func commandHook(publisher, event string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": publisher,
		"args":    []any{"--provider", "claude-code", "--event", event},
		"timeout": 10,
	}
}

func CommandFor(event string) string {
	return fmt.Sprintf("discobox-hook-publish --provider claude-code --event %s", event)
}

// conversationState carries a claude session ID between Prompt calls.
type conversationState struct {
	SessionID string `json:"session_id,omitempty"`
}

// Prompt implements harness.Converser.
// It runs claude non-interactively using --input-format stream-json and
// --output-format json, resuming the previous session when state is non-nil.
func (Driver) Prompt(ctx context.Context, prompt string, state []byte) (string, []byte, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}

	var s conversationState
	if len(state) > 0 {
		_ = json.Unmarshal(state, &s)
	}
	if s.SessionID != "" {
		args = append(args, "--resume", s.SessionID)
	}

	// claude --input-format stream-json reads JSONL messages from stdin.
	msg, err := json.Marshal(map[string]string{"role": "user", "content": prompt})
	if err != nil {
		return "", nil, fmt.Errorf("encode claude input: %w", err)
	}
	input := append(msg, '\n')

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("claude: %w", err)
	}

	// --output-format json emits a single JSON result object:
	// {"type":"result","subtype":"success","result":"...","session_id":"...","is_error":false,...}
	var result struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return "", nil, fmt.Errorf("parse claude output: %w", err)
	}
	if result.IsError {
		return "", nil, fmt.Errorf("claude: %s", result.Result)
	}

	newState, err := json.Marshal(conversationState{SessionID: result.SessionID})
	return result.Result, newState, err
}
