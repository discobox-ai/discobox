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
	TerminalIDEnv = "DISCOBOX_TERMINAL_ID"
	SocketEnv     = "DISCOBOX_HOOK_SOCKET"
)

type Agent struct {
	ID      string
	Name    string
	Command []string
}

type InstallRequest struct {
	Agent            Agent
	Workdir          string
	Env              map[string]string
	PublisherCommand string
	ManagedRoot      string
}

type Driver interface {
	ID() string
	Install(context.Context, InstallRequest) error
}

func PublisherCommand(req InstallRequest) string {
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
	return os.WriteFile(path, data, 0o600)
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
