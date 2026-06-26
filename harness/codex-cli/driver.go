package codexcli

import (
	"context"
	"fmt"

	"github.com/obot-platform/discobox/harness"
)

const SystemHooksPath = "/.codex/hooks.json"

var Events = []string{
	"SessionStart", "PreToolUse", "PermissionRequest", "PostToolUse",
	"UserPromptSubmit", "PreCompact", "PostCompact", "SubagentStart",
	"SubagentStop", "Stop",
}

type Driver struct{}

func (Driver) ID() string { return "codex-cli" }

func (Driver) Install(_ context.Context, req harness.InstallRequest) error {
	path := harness.ManagedPath(req.ManagedRoot, SystemHooksPath)
	return harness.MergeJSONFile(path, func(config map[string]any) {
		hooks := harness.SetJSONMap(config, "hooks")
		for _, event := range Events {
			harness.UpsertEventCommandHook(hooks, event, "*", commandHook(harness.PublisherCommand(req), event))
		}
	})
}

func commandHook(publisher, event string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": fmt.Sprintf("%s --provider codex-cli --event %s", publisher, event),
		"timeout": 10,
	}
}
