package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/obot-platform/discobox/harness"
)

const (
	ManagedConfigDir  = "/etc/opencode"
	ManagedConfigPath = "/etc/opencode/opencode.json"
	PluginPath        = "/etc/opencode/plugins/discobox-hook-publish.js"
)

type Driver struct{}

func (Driver) ID() string { return "opencode" }

func (Driver) Install(_ context.Context, req harness.InstallRequest) error {
	configDir := harness.ManagedPath(req.ManagedRoot, ManagedConfigDir)
	harness.SetEnv(req.Env, "OPENCODE_CONFIG_DIR", configDir)
	if err := harness.MergeJSONFile(harness.ManagedPath(req.ManagedRoot, ManagedConfigPath), func(config map[string]any) {
		if _, ok := config["$schema"]; !ok {
			config["$schema"] = "https://opencode.ai/config.json"
		}
	}); err != nil {
		return err
	}
	pluginPath := harness.ManagedPath(req.ManagedRoot, PluginPath)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(pluginPath, []byte(pluginSource(harness.PublisherCommand(req))), harness.ManagedFileMode); err != nil {
		return err
	}
	return os.Chmod(pluginPath, harness.ManagedFileMode)
}

func pluginSource(publisher string) string {
	argv := jsArray(strings.Fields(publisher))
	return `import { spawn } from "node:child_process"

const baseCommand = ` + argv + `

async function publish(event, payload) {
  const args = [...baseCommand.slice(1), "--provider", "opencode", "--event", event]
  const child = spawn(baseCommand[0], args, {
    stdio: ["pipe", "ignore", "ignore"],
  })
  child.stdin.end(JSON.stringify(payload ?? {}))
  await new Promise((resolve) => child.on("close", resolve))
}

export const DiscoboxHookPublish = async () => {
  return {
    event: async ({ event }) => {
      await publish(event.type || "event", event)
    },
    "tool.execute.before": async (input, output) => {
      await publish("tool.execute.before", { input, output })
    },
    "tool.execute.after": async (input, output) => {
      await publish("tool.execute.after", { input, output })
    },
    "shell.env": async (input, output) => {
      await publish("shell.env", { input, output })
    },
  }
}
`
}

func jsArray(values []string) string {
	if len(values) == 0 {
		values = []string{"discobox-hook-publish"}
	}
	out := "["
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return out + "]"
}

// conversationState carries an opencode session ID between Prompt calls.
type conversationState struct {
	SessionID string `json:"session_id,omitempty"`
}

// Prompt implements harness.Converser.
// It runs "opencode run --format json [--session <id>] <message>" and parses
// the JSON event stream for the session ID and final assistant response.
func (Driver) Prompt(ctx context.Context, prompt string, state []byte) (string, []byte, error) {
	var s conversationState
	if len(state) > 0 {
		_ = json.Unmarshal(state, &s)
	}

	args := []string{"run", "--format", "json", "--dangerously-skip-permissions"}
	if s.SessionID != "" {
		args = append(args, "--session", s.SessionID)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("opencode: %w", err)
	}

	result, sessionID := parseOpencodeOutput(out)
	if sessionID == "" {
		sessionID = s.SessionID
	}
	if result == "" {
		return "", nil, fmt.Errorf("opencode: no assistant response in output")
	}

	newState, err := json.Marshal(conversationState{SessionID: sessionID})
	return result, newState, err
}

// parseOpencodeOutput scans the JSON event stream from "opencode run --format json"
// for the session ID and the final assistant message text.
//
// opencode emits JSONL events. Known shapes include:
//
//	{"type":"SessionCreated","properties":{"info":{"id":"..."}}}
//	{"type":"AssistantMessage","properties":{"sessionID":"...","message":{"role":"assistant","parts":[{"type":"text","text":"..."}]}}}
func parseOpencodeOutput(data []byte) (text, sessionID string) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type       string `json:"type"`
			Properties struct {
				SessionID string `json:"sessionID"`
				Info      struct {
					ID string `json:"id"`
				} `json:"info"`
				Message struct {
					Role  string `json:"role"`
					Parts []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if id := event.Properties.Info.ID; id != "" {
			sessionID = id
		}
		if id := event.Properties.SessionID; id != "" {
			sessionID = id
		}
		if event.Properties.Message.Role == "assistant" {
			for _, part := range event.Properties.Message.Parts {
				if part.Type == "text" {
					text = part.Text
				}
			}
		}
	}
	return
}
