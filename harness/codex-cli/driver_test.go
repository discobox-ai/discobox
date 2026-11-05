package codexcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/featuretest"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

// The list is Codex's own hook lifecycle table at
// https://developers.openai.com/codex/hooks (the repository ships no
// equivalent Markdown). Last checked against 0.155.1, which defines 12
// events. Interrupt is the one that does not fire for subagents, and
// SessionEnd is the one that fires only for the main thread. See the
// claude-code twin of this test for why the list goes stale.
func TestImageOwnedHooksPublishEveryCodexLifecycleEvent(t *testing.T) {
	raw, err := os.ReadFile("hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"SessionStart", "SessionEnd", "PreToolUse", "PermissionRequest",
		"PostToolUse", "UserPromptSubmit", "PreCompact", "PostCompact",
		"SubagentStart", "SubagentStop", "Stop", "Interrupt",
	}
	if len(config.Hooks) != len(wantEvents) {
		t.Fatalf("events = %d, want %d: %#v", len(config.Hooks), len(wantEvents), config.Hooks)
	}
	for _, event := range wantEvents {
		groups := config.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("event %s does not have exactly one publisher: %#v", event, groups)
		}
		hook := groups[0].Hooks[0]
		wantCommand := "discobox-hook-publish --provider codex-cli --event " + event
		if hook.Type != "command" || hook.Command != wantCommand {
			t.Errorf("event %s hook = %#v", event, hook)
		}
		if hook.Timeout <= 0 || hook.Timeout > 3 {
			t.Errorf("event %s timeout = %d, want 1..3 seconds", event, hook.Timeout)
		}
	}
}

func TestSystemConfigPublishesCodexThreadTitle(t *testing.T) {
	raw, err := os.ReadFile("system-config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `terminal_title = ["activity", "thread-title"]`) {
		t.Fatalf("system config does not publish activity and thread title: %s", raw)
	}
}

// TestImageDeclaresFileDeliveredAuth pins the half of the contract the image
// owns: codex authenticates from a file, so neither credential may be exported
// as an environment variable, and each is an alternative to the other.
func TestImageDeclaresFileDeliveredAuth(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Harness struct {
			Files   []harness.File `json:"files"`
			Secrets []struct {
				Name       string `json:"name"`
				Required   bool   `json:"required"`
				OneOfGroup string `json:"oneOfGroup"`
				Delivery   string `json:"delivery"`
			} `json:"secrets"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	wantSecrets := map[string]bool{"OPENAI_API_KEY": false, "CODEX_OAUTH_TOKEN": false}
	for _, secret := range image.Harness.Secrets {
		if _, ok := wantSecrets[secret.Name]; !ok {
			continue
		}
		wantSecrets[secret.Name] = true
		if secret.Delivery != harness.SecretDeliveryFile {
			t.Errorf("secret %s delivery = %q, want %q: codex's interactive TUI reads no credential env var",
				secret.Name, secret.Delivery, harness.SecretDeliveryFile)
		}
		if !secret.Required || secret.OneOfGroup == "" {
			t.Errorf("secret %s is not a required alternative: required=%v group=%q", secret.Name, secret.Required, secret.OneOfGroup)
		}
	}
	for name, found := range wantSecrets {
		if !found {
			t.Errorf("image declares no %s secret", name)
		}
	}
	// The credential file is the sign-in's to write. A baseline one
	// would land in every sandbox as a credential-shaped file with nothing
	// behind it, which codex reads as "signed in" -- and then fails every
	// request with no way to sign in from the session.
	for _, file := range image.Harness.Files {
		if file.Path == ".codex/auth.json" {
			t.Errorf("image declares a baseline %s, which authenticates a sandbox with nothing", file.Path)
		}
	}
}

func TestImageLaunchesCodexWithSourceScopedMemory(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Harness struct {
			RunCommand      []string `json:"runCommand"`
			RelaunchCommand []string `json:"relaunchCommand"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	// The manifest names no command: the launcher is installed under the
	// conventional name and the runtime types that (ADR 0086 §3). Declaring one
	// here would be an override, and would silently outrank the convention.
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	if got, want := featuretest.Load(t, ".").Installs("launch.sh"), "/usr/local/bin/"+harness.RunCommand; got != want {
		t.Fatalf("the feature installs the launcher at %q, want %q", got, want)
	}
	scriptBytes, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		// The resume half of the convention, and the prompt that trails it on
		// every launch (ADR 0086 §4): a resumed session already carries the
		// prompt, so the launcher drops it rather than re-sending it.
		harness.ResumeFlag,
		"set -- resume --last",
		// Codex sends a copy to the user's terminal only in an SSH session;
		// without it every copy lands on the box's own X clipboard.
		`export SSH_CONNECTION="${SSH_CONNECTION:-`,
		"/.discobox/data-per-source/primary",
		"users/$(id -u)",
		"harnesses/codex/memories",
		`${CODEX_HOME:-$HOME/.codex}`,
		`mountpoint -q "$LOCAL_MEMORIES"`,
		`mount --bind "$SHARED_MEMORIES" "$LOCAL_MEMORIES"`,
		`exec codex "$@"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("launch script is missing %q", required)
		}
	}
	if strings.Contains(script, `ln -s`) {
		t.Fatal("launch script uses a symlink for a memory root Codex requires to be a real directory")
	}
	config, err := os.ReadFile("system-config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "memories = true") {
		t.Fatal("system config does not enable Codex memory consolidation")
	}
	for _, setting := range []string{
		"generate_memories = true",
		"use_memories = true",
		"min_rate_limit_remaining_percent = 0",
	} {
		if !strings.Contains(string(config), setting) {
			t.Errorf("system config does not set %q", setting)
		}
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3): the command is typed, so the login shell splits the
// prompt before the launcher sees it, and the launcher hands `codex` the one
// prompt it takes as a positional rather than its first word.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"fix the failing tests"}},
		{"no prompt passes none", nil, nil},
		// A resumed session already carries the prompt (ADR 0086 §4), so the
		// launcher replaces it rather than sending it a second time.
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"resume", "--last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "codex", nil, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("codex argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestLaunchKeysMemoriesByUID runs the launcher against a source-data mount:
// memories land in the running uid's partition, so sandboxes on one source
// share them only when they agree on a uid, and memories written before the
// partition existed are carried over to the uid that owns them. The bind itself
// needs root, so the stubbed sudo fails it and Codex still launches.
func TestLaunchKeysMemoriesByUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the partition is named by a POSIX uid, and os.Getuid is -1 on Windows")
	}
	sourceData := t.TempDir()
	legacy := filepath.Join(sourceData, "harnesses", "codex", "memories")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "memory.md"), []byte("remembered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	memories := filepath.Join(sourceData, "users", strconv.Itoa(os.Getuid()), "harnesses", "codex", "memories")

	if got, want := launchertest.RunLauncher(t, "codex", map[string]string{launchertest.SourceDataPath: sourceData}, []string{"hi"}), []string{"hi"}; !slices.Equal(got, want) {
		t.Fatalf("codex argv = %#v, want %#v", got, want)
	}
	carried, err := os.ReadFile(filepath.Join(memories, "memory.md"))
	if err != nil || string(carried) != "remembered\n" {
		t.Fatalf("legacy memories were not carried over: %q, %v", carried, err)
	}
}

// The mapping table and this image's hook config are one thing in two files,
// and nothing but this test keeps them together. Every event the image
// publishes must either have a canonical name or be listed here as having
// none — so adding a hook without deciding what it is called across harnesses
// fails rather than quietly recording a hook nothing portable can match
// (ADR 0146 §3).
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	raw, err := os.ReadFile("hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	// Codex events Claude Code has no word for. Interrupt is the only one:
	// Claude Code ends a turn with Stop or StopFailure and has no hook for the
	// user interrupting one. Delete an entry here the day Claude Code grows a
	// counterpart, and add the mapping instead.
	noCanonicalName := []string{"Interrupt"}

	for event := range config.Hooks {
		canonical := harness.CanonicalHookEvent(Driver{}.ID(), event)
		if slices.Contains(noCanonicalName, event) {
			if canonical != "" {
				t.Errorf("event %s now maps to %q; drop it from noCanonicalName", event, canonical)
			}
			continue
		}
		if canonical == "" {
			t.Errorf("event %s has no canonical name and is not listed as having none", event)
		}
	}
	for _, event := range noCanonicalName {
		if _, ok := config.Hooks[event]; !ok {
			t.Errorf("noCanonicalName lists %s, which this image does not publish", event)
		}
	}
}
