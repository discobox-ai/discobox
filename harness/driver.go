// Package harness installs coding-agent hook integrations for sandbox terminals.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	TerminalIDEnv   = "DISCOBOX_TERMINAL_ID"
	SocketEnv       = "DISCOBOX_HOOK_SOCKET"
	ManagedFileMode = 0o644
)

type Agent struct {
	ID      string
	Name    string
	Command []string
}

// Definition is a harness's built-in agent-config template: how to install and
// run the coding agent, how to resume its previous session on a restart, and any
// files to seed into the agent's home directory. It is the single source of
// truth for an agent's harness-specific defaults; the control plane converts it
// into a project-scoped agent config.
type Definition struct {
	ID              string
	Name            string
	Description     string
	InstallCommand  []string
	RunCommand      []string
	RelaunchCommand []string
	Files           []File
	Secrets         []Secret
}

// File is a file to write into the agent's home directory when the agent is
// installed.
type File struct {
	Path       string
	Content    string
	CreateOnly bool
	Template   bool
}

// Secret declares an environment variable the agent expects, and whether it is
// required for the agent to run. Optional secrets are used when present but do
// not block the agent from launching.
//
// OneOfGroup ties a required secret to a set of alternatives: required secrets
// sharing a group form an at-least-one requirement, satisfied when any member is
// present (e.g. an API key or an OAuth token). Ungrouped required secrets each
// must be satisfied independently.
type Secret struct {
	Name       string
	Required   bool
	OneOfGroup string
}

// HookInstallRequest is the input to installing a harness's hook integration.
// It is unrelated to Definition.InstallCommand, which installs the agent CLI
// itself.
type HookInstallRequest struct {
	Agent            Agent
	Workdir          string
	Env              map[string]string
	PublisherCommand string
	ManagedRoot      string
}

type Driver interface {
	ID() string
	// Definition returns the harness's built-in agent-config template.
	Definition() Definition
	// InstallHooks wires the agent's lifecycle hook integration into its managed
	// config. It does not install the agent CLI (see Definition.InstallCommand).
	InstallHooks(context.Context, HookInstallRequest) error
}

// Converser is implemented by drivers that support automated multi-turn conversations.
// Prompt sends a user message and returns the final assistant response.
// state is an opaque blob from the previous call; nil starts a new conversation.
// The returned state must be passed to the next call to continue the conversation.
type Converser interface {
	Prompt(ctx context.Context, prompt string, state []byte) (result string, newState []byte, err error)
}

func PublisherCommand(req HookInstallRequest) string {
	if strings.TrimSpace(req.PublisherCommand) != "" {
		return strings.TrimSpace(req.PublisherCommand)
	}
	return "discobox-hook-publish"
}

func ManagedPath(root, path string) string {
	if strings.TrimSpace(root) == "" {
		return filepath.Clean(path)
	}
	return filepath.Join(filepath.Clean(root), strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
}

func MergeJSONFile(path string, apply func(map[string]any)) error {
	current := map[string]any{}
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	apply(current)
	return WriteJSONFile(path, current)
}

func WriteJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, ManagedFileMode); err != nil {
		return err
	}
	return os.Chmod(path, ManagedFileMode)
}

func JSONMap(value any) map[string]any {
	if decoded, ok := value.(map[string]any); ok && decoded != nil {
		return decoded
	}
	return map[string]any{}
}

func SetJSONMap(parent map[string]any, key string) map[string]any {
	current := JSONMap(parent[key])
	parent[key] = current
	return current
}

func UpsertEventCommandHook(hooks map[string]any, event, matcher string, hook map[string]any) {
	if matcher == "" {
		matcher = "*"
	}
	entries, _ := hooks[event].([]any)
	for i, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok || stringValue(entry["matcher"]) != matcher {
			continue
		}
		entryHooks, _ := entry["hooks"].([]any)
		entry["hooks"] = upsertHook(entryHooks, hook)
		entries[i] = entry
		hooks[event] = entries
		return
	}
	hooks[event] = append(entries, map[string]any{
		"matcher": matcher,
		"hooks":   []any{hook},
	})
}

func upsertHook(hooks []any, hook map[string]any) []any {
	for i, rawExisting := range hooks {
		existing, ok := rawExisting.(map[string]any)
		if !ok || !sameHookIdentity(existing, hook) {
			continue
		}
		hooks[i] = hook
		return hooks
	}
	return append(hooks, hook)
}

func sameHookIdentity(left, right map[string]any) bool {
	return stringValue(left["type"]) == stringValue(right["type"]) &&
		stringValue(left["command"]) == stringValue(right["command"]) &&
		stringSliceValue(left["args"]) == stringSliceValue(right["args"])
}

func stringValue(value any) string {
	if decoded, ok := value.(string); ok {
		return decoded
	}
	return ""
}

func stringSliceValue(value any) string {
	values, ok := value.([]any)
	if !ok {
		return ""
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, stringValue(value))
	}
	return strings.Join(out, "\x00")
}

func SetEnv(env map[string]string, key, value string) {
	if env == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return
	}
	env[key] = value
}
