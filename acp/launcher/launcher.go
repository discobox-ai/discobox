// Package launcher resolves and starts ACP agent commands.
package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/obot-platform/discobox/acp/registry"
)

// Command is a resolved ACP process command.
type Command struct {
	AgentID string
	Method  string
	Command string
	Args    []string
	Env     map[string]string
}

// Runtime records an ACP process launched by discobox-acp.
type Runtime struct {
	AgentID   string    `json:"agent_id"`
	PID       int       `json:"pid"`
	AgentPID  int       `json:"agent_pid,omitempty"`
	Socket    string    `json:"socket,omitempty"`
	Command   string    `json:"command"`
	Args      []string  `json:"args,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Resolve returns an on-demand launch command from registry distribution
// metadata. Package-manager distributions are preferred because they do not
// require Discobox-owned installation state.
func Resolve(agent registry.Agent) (Command, error) {
	if agent.Distribution.NPX != nil {
		target := agent.Distribution.NPX
		args := append([]string{"--yes", target.Package}, target.Args...)
		return Command{AgentID: agent.ID, Method: "npx", Command: "npx", Args: args, Env: target.Env}, nil
	}
	if agent.Distribution.UVX != nil {
		target := agent.Distribution.UVX
		args := append([]string{target.Package}, target.Args...)
		return Command{AgentID: agent.ID, Method: "uvx", Command: "uvx", Args: args, Env: target.Env}, nil
	}
	_, _, ok, err := agent.BinaryForCurrentPlatform()
	if err != nil {
		return Command{}, err
	}
	if ok {
		return Command{}, fmt.Errorf("agent %q only advertises a binary archive for this platform; on-demand binary archive launch is not implemented", agent.ID)
	}
	return Command{}, fmt.Errorf("agent %q has no launchable distribution", agent.ID)
}

// ExecCommand constructs an exec.Cmd with inherited environment plus registry env.
func ExecCommand(ctx context.Context, command Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command.Command, command.Args...)
	cmd.Env = os.Environ()
	if len(command.Env) > 0 {
		for k, v := range command.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd
}

// RecordRuntime writes the runtime metadata for a started supervisor process.
func RecordRuntime(agentID string, supervisorPID int, agentCmd *exec.Cmd, socket string) error {
	rt := Runtime{
		AgentID:   agentID,
		PID:       supervisorPID,
		Socket:    socket,
		StartedAt: time.Now().UTC(),
	}
	if agentCmd != nil {
		rt.Command = agentCmd.Path
		rt.Args = agentCmd.Args[1:]
		if agentCmd.Process != nil {
			rt.AgentPID = agentCmd.Process.Pid
		}
	}
	path := runtimeRecordPath(agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rt, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// ClearRuntime removes the runtime metadata for agentID.
func ClearRuntime(agentID string) error {
	err := os.Remove(runtimeRecordPath(agentID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RuntimeStatus returns the latest Discobox-recorded runtime state.
func RuntimeStatus(agentID string) (*Runtime, bool, bool, error) {
	path := runtimeRecordPath(agentID)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	var rt Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		return nil, false, false, fmt.Errorf("decode runtime metadata: %w", err)
	}
	running := processExists(rt.PID)
	return &rt, running, !running, nil
}

func runtimeRecordPath(agentID string) string {
	return filepath.Join(runtimeRoot(), agentID+".json")
}

// SocketPath returns the supervisor Unix socket path for an agent.
func SocketPath(agentID string) string {
	return filepath.Join(runtimeRoot(), agentID+".sock")
}

func runtimeRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "discobox", "acp")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			stateHome = filepath.Join(home, ".local", "state")
		}
	}
	return filepath.Join(stateHome, "discobox", "acp", "run")
}
