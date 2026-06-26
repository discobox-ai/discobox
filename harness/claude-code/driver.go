package claudecode

import (
	"context"
	"fmt"

	"github.com/obot-platform/discobox/harness"
)

const ManagedSettingsPath = "/etc/claude-code/managed-settings.json"

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

func (Driver) Install(_ context.Context, req harness.InstallRequest) error {
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
