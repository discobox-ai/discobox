package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	claudecode "github.com/discobox-ai/discobox/harness/claude-code"
	codexcli "github.com/discobox-ai/discobox/harness/codex-cli"
)

func TestDefinitionsCoverKnownHarnesses(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != len(DefaultDrivers()) {
		t.Fatalf("definitions = %d, want one per default driver (%d)", len(definitions), len(DefaultDrivers()))
	}
	byID := map[string]harness.Definition{}
	for _, definition := range definitions {
		if definition.ID == "" || definition.Name == "" || definition.Image == "" {
			t.Fatalf("definition %#v must identify an image", definition)
		}
		byID[definition.ID] = definition
	}
	// Configure is what enables the interactive setup flow, so it tracks
	// whether the harness has credentials to collect — not whether it is a
	// harness. `shell` has none, and `configure` refuses it on those grounds.
	for _, id := range []string{"claude-code", "codex"} {
		definition, ok := byID[id]
		if !ok {
			t.Fatalf("missing definition %q", id)
		}
		if definition.Configure == nil {
			t.Fatalf("definition %q must be configurable", id)
		}
	}
	shell, ok := byID["shell"]
	if !ok {
		t.Fatal("missing definition \"shell\"; it is the end of the harness resolution chain")
	}
	if shell.Configure != nil {
		t.Fatalf("shell definition = %#v, want no configure flow: it collects no credentials", shell)
	}
}

func TestInstallerWritesManagedHarnessFiles(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{}
	installer := Installer{
		Drivers:     DefaultDrivers(),
		ManagedRoot: root,
	}
	err := installer.InstallHooks(context.Background(), harness.HookInstallRequest{
		Env: env,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	assertJSONPath(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), "hooks")
	assertJSONPath(t, filepath.Join(root, ".codex/hooks.json"), "hooks")
	assertMode(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), 0o644)
	assertMode(t, filepath.Join(root, ".codex/hooks.json"), 0o644)
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
	installer := Installer{
		Drivers:     DefaultDrivers(),
		ManagedRoot: root,
	}
	req := harness.HookInstallRequest{Env: map[string]string{}}
	if err := installer.InstallHooks(context.Background(), req); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first := readJSONFiles(t, root)
	if err := installer.InstallHooks(context.Background(), req); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second := readJSONFiles(t, root)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("install is not idempotent\nfirst: %#v\nsecond: %#v", first, second)
	}
	assertMode(t, filepath.Join(root, "etc/claude-code/managed-settings.json"), 0o644)
	assertMode(t, filepath.Join(root, ".codex/hooks.json"), 0o644)

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
}

func TestDriverForHarnessSelectsByImageType(t *testing.T) {
	cases := []struct {
		harness harness.Harness
		want    string
	}{
		{harness: harness.Harness{TypeID: "claude-code"}, want: claudecode.Driver{}.ID()},
		{harness: harness.Harness{TypeID: "codex"}, want: codexcli.Driver{}.ID()},
	}
	for _, tc := range cases {
		got := DriverForHarness(tc.harness)
		if len(got) != 1 || got[0].ID() != tc.want {
			t.Fatalf("DriverForHarness(%#v) = %#v, want %s", tc.harness, got, tc.want)
		}
	}
}

func TestDriverForHarnessFallsBackForUnknownType(t *testing.T) {
	for _, harness := range []harness.Harness{
		{},
		{TypeID: "unknown"},
		{ID: "claude-code"},
	} {
		if got := DriverForHarness(harness); len(got) != len(DefaultDrivers()) {
			t.Fatalf("DriverForHarness(%#v) = %#v, want all default drivers", harness, got)
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
	// Windows has no POSIX permission bits: the perm argument to os.WriteFile
	// maps only to the read-only attribute, so Perm() reads back 0666 whatever
	// was asked for. The property here is a POSIX one, carried on Windows by
	// the ACL inherited from the parent directory, which this cannot express.
	if runtime.GOOS == "windows" {
		return
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
