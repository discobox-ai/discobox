package codexcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/discobox-ai/discobox/harness"
)

type Driver struct{}

func (Driver) ID() string { return "codex-cli" }

func (Driver) Definition() harness.Definition {
	return harness.Definition{
		ID: "codex", Name: "Codex", Description: "OpenAI Codex coding harness.",
		Image: harness.ImageRef("discobox-harness-codex"), Configure: &harness.Configure{},
	}
}

// conversationState carries a codex session ID between Prompt calls.
type conversationState struct {
	SessionID string `json:"session_id,omitempty"`
}

// Prompt implements harness.Converser.
// It runs codex non-interactively, using "codex exec resume" for continued
// sessions. The prompt is passed via stdin (using "-" as the prompt argument).
// The final response is captured via --output-last-message to a temp file.
// The session ID for continuation is parsed from the JSONL event stream.
func (Driver) Prompt(ctx context.Context, prompt string, state []byte) (string, []byte, error) {
	tmp, err := os.CreateTemp("", "codex-out-*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("codex: create temp file: %w", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	var s conversationState
	if len(state) > 0 {
		_ = json.Unmarshal(state, &s)
	}

	var args []string
	if s.SessionID != "" {
		// Resume existing session; "-" tells codex to read the new prompt from stdin.
		args = []string{
			"exec", "resume", s.SessionID, "-",
			"--json",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
			"--output-last-message", tmp.Name(),
		}
	} else {
		// New session; "-" tells codex to read the prompt from stdin.
		args = []string{
			"exec", "-",
			"--json",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
			"--output-last-message", tmp.Name(),
		}
	}

	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdin = strings.NewReader(prompt)
	jsonOut, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("codex: %w", err)
	}

	// The final assistant response is written to the temp file by --output-last-message.
	resultBytes, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", nil, fmt.Errorf("codex: read output: %w", err)
	}

	sessionID := parseCodexSessionID(jsonOut)
	if sessionID == "" {
		sessionID = s.SessionID // keep existing if we can't find a new one
	}

	newState, err := json.Marshal(conversationState{SessionID: sessionID})
	return strings.TrimSpace(string(resultBytes)), newState, err
}

// parseCodexSessionID scans the JSONL event stream from codex --json for a session ID.
// codex emits events like: {"type":"session","session":{"id":"..."}} or {"session_id":"..."}.
func parseCodexSessionID(data []byte) string {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if id, _ := event["session_id"].(string); id != "" {
			return id
		}
		if sess, _ := event["session"].(map[string]any); sess != nil {
			if id, _ := sess["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}
