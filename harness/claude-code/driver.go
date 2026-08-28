package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/discobox-ai/discobox/harness"
)

type Driver struct{}

func (Driver) ID() string { return "claude-code" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "claude-code", Name: "Claude Code", Description: "Anthropic Claude Code coding harness.",
		Image: harness.ImageRef("discobox-harness-claude-code"), Configure: &harness.Configure{},
	}
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
