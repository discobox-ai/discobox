package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/obot-platform/discobox/harness"
	claudecode "github.com/obot-platform/discobox/harness/claude-code"
	codexcli "github.com/obot-platform/discobox/harness/codex-cli"
	"github.com/obot-platform/discobox/harness/opencode"
)

func TestInstallerWritesManagedHarnessFiles(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{}
	installer := Installer{
		Drivers:     DefaultDrivers(),
		ManagedRoot: root,
	}
	err := installer.Install(context.Background(), harness.InstallRequest{
		Env: env,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	assertJSONPath(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), "hooks")
	assertJSONPath(t, filepath.Join(root, ".codex/hooks.json"), "hooks")
	assertJSONPath(t, filepath.Join(root, "etc/opencode/opencode.json"), "$schema")
	assertMode(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), 0o644)
	assertMode(t, filepath.Join(root, ".codex/hooks.json"), 0o644)
	assertMode(t, filepath.Join(root, "etc/opencode/opencode.json"), 0o644)
	if env["OPENCODE_CONFIG_DIR"] != filepath.Join(root, "etc/opencode") {
		t.Fatalf("OPENCODE_CONFIG_DIR = %q", env["OPENCODE_CONFIG_DIR"])
	}
	pluginPath := filepath.Join(root, "etc/opencode/plugins/discobox-hook-publish.js")
	if _, err := os.Stat(pluginPath); err != nil {
		t.Fatalf("opencode plugin missing: %v", err)
	}
	assertMode(t, pluginPath, 0o644)
}

func TestInstallerPreservesExistingConfigAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), `{
  "permissions": {"allow": ["Bash(git status)"]},
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {"type": "command", "command": "existing-claude-hook"}
        ]
      }
    ]
  }
}`)
	writeFile(t, filepath.Join(root, ".codex/hooks.json"), `{
  "other": true,
  "hooks": {
    "Stop": [
      {
        "matcher": "*",
        "hooks": [
          {"type": "command", "command": "existing-codex-hook"}
        ]
      }
    ]
  }
}`)
	writeFile(t, filepath.Join(root, "etc/opencode/opencode.json"), `{
  "$schema": "https://opencode.ai/config.json",
  "theme": "system"
}`)

	installer := Installer{
		Drivers:     DefaultDrivers(),
		ManagedRoot: root,
	}
	req := harness.InstallRequest{Env: map[string]string{}}
	if err := installer.Install(context.Background(), req); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first := readJSONFiles(t, root)
	if err := installer.Install(context.Background(), req); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second := readJSONFiles(t, root)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("install is not idempotent\nfirst: %#v\nsecond: %#v", first, second)
	}
	assertMode(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), 0o644)
	assertMode(t, filepath.Join(root, ".codex/hooks.json"), 0o644)
	assertMode(t, filepath.Join(root, "etc/opencode/opencode.json"), 0o644)

	claude := first["etc/claude-code/managed-settings.json"]
	if _, ok := claude["permissions"].(map[string]any); !ok {
		t.Fatalf("claude settings lost existing permissions: %#v", claude)
	}
	assertCommandHook(t, claude, "PreToolUse", "existing-claude-hook")
	assertCommandHook(t, claude, "PreToolUse", "discobox-hook-publish")

	codex := first[".codex/hooks.json"]
	if codex["other"] != true {
		t.Fatalf("codex settings lost existing key: %#v", codex)
	}
	assertCommandHook(t, codex, "Stop", "existing-codex-hook")
	assertCommandHook(t, codex, "Stop", "discobox-hook-publish --provider codex-cli --event Stop")

	opencode := first["etc/opencode/opencode.json"]
	if opencode["theme"] != "system" {
		t.Fatalf("opencode config lost existing theme: %#v", opencode)
	}
}

func TestDriverForAgentSelectsKnownAgents(t *testing.T) {
	cases := []struct {
		agent harness.Agent
		want  string
	}{
		{agent: harness.Agent{ID: "claude-code"}, want: claudecode.Driver{}.ID()},
		{agent: harness.Agent{ID: "codex"}, want: codexcli.Driver{}.ID()},
		{agent: harness.Agent{Command: []string{"opencode"}}, want: opencode.Driver{}.ID()},
	}
	for _, tc := range cases {
		got := DriverForAgent(tc.agent)
		if len(got) != 1 || got[0].ID() != tc.want {
			t.Fatalf("DriverForAgent(%#v) = %#v, want %s", tc.agent, got, tc.want)
		}
	}
}

func assertJSONPath(t *testing.T, path, key string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if _, ok := decoded[key]; !ok {
		t.Fatalf("%s missing key %q: %s", path, key, data)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSONFiles(t *testing.T, root string) map[string]map[string]any {
	t.Helper()
	paths := []string{
		"etc/claude-code/managed-settings.json",
		".codex/hooks.json",
		"etc/opencode/opencode.json",
	}
	result := map[string]map[string]any{}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		result[path] = decoded
	}
	return result
}

func assertCommandHook(t *testing.T, config map[string]any, event, command string) {
	t.Helper()
	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks in %#v", config)
	}
	entries, ok := hooks[event].([]any)
	if !ok {
		t.Fatalf("missing event %s in %#v", event, hooks)
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		entryHooks, _ := entry["hooks"].([]any)
		for _, rawHook := range entryHooks {
			hook, ok := rawHook.(map[string]any)
			if ok && hook["command"] == command {
				return
			}
		}
	}
	t.Fatalf("missing command hook %q for event %s in %#v", command, event, config)
}
