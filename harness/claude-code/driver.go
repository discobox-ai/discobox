package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/discobox-ai/discobox/harness"
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

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "claude-code", Name: "Claude Code", Description: "Anthropic Claude Code coding harness.",
		Image: "discobox-harness-claude-code:local", Configure: &harness.Configure{},
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

// stateByEvent maps a claude-code hook event to the session state it defines.
// Events absent from this map are informational only: they update
// lastEvent/lastEventAt but do not change the derived state, deferring the
// decision to the next state-defining event found scanning backward.
var stateByEvent = map[string]string{
	"PermissionRequest": harness.SessionStateNeedsInput,
	"Notification":      harness.SessionStateNeedsInput,
	"Elicitation":       harness.SessionStateNeedsInput,

	"Stop":         harness.SessionStateIdle,
	"StopFailure":  harness.SessionStateIdle,
	"SessionEnd":   harness.SessionStateIdle,
	"TeammateIdle": harness.SessionStateIdle,

	"SessionStart":       harness.SessionStateRunning,
	"UserPromptSubmit":   harness.SessionStateRunning,
	"PreToolUse":         harness.SessionStateRunning,
	"PostToolUse":        harness.SessionStateRunning,
	"PostToolUseFailure": harness.SessionStateRunning,
	"PostToolBatch":      harness.SessionStateRunning,
	"SubagentStart":      harness.SessionStateRunning,
	"TaskCreated":        harness.SessionStateRunning,
}

// DeriveSessionState implements harness.SessionStateDeriver. hooks must be
// ascending by CreatedAt (as harness hook queries already return them); it
// scans backward from the most recent event and returns the first
// state-defining event's mapped state, so the most recent state-defining
// event always wins over an older one, regardless of what informational
// events fired in between.
func (Driver) DeriveSessionState(hooks []harness.HookRecord) (state, lastEvent string, lastEventAt time.Time) {
	if len(hooks) == 0 {
		return "", "", time.Time{}
	}
	last := hooks[len(hooks)-1]
	lastEvent, lastEventAt = last.Event, last.CreatedAt
	for i := len(hooks) - 1; i >= 0; i-- {
		if mapped, ok := stateByEvent[hooks[i].Event]; ok {
			return mapped, lastEvent, lastEventAt
		}
	}
	return "", lastEvent, lastEventAt
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
