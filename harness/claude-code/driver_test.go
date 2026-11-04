package claudecode

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

func TestImageLaunchesClaudeWithSourceScopedMemory(t *testing.T) {
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
		`exec claude "$@"`,
		// The resume half of the convention, and the prompt that trails it
		// on every launch (ADR 0086 §4): a resumed session already carries
		// the prompt, so the launcher drops it rather than re-sending it.
		harness.ResumeFlag,
		"set -- --continue",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("launch script is missing %q", required)
		}
	}
}

// Where Claude Code keeps its memory and its temp tree is the sandbox's
// environment, not the launcher's: a `claude` typed into any shell has to find
// the same memory and the same scratchpads as the harness terminal does. The
// memory path names the uid, so the image's static managed settings leave it
// to the drop-in the launcher records (TestLaunchKeysMemoriesByUID); an
// unkeyed path there would be the one every uid collides on.
func TestClaudeStorageIsEnvironmental(t *testing.T) {
	raw, err := os.ReadFile("managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		AutoMemoryDirectory *string `json:"autoMemoryDirectory"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.AutoMemoryDirectory != nil {
		t.Errorf("managed-settings.json sets autoMemoryDirectory %q, which no uid partition can be named in", *settings.AutoMemoryDirectory)
	}

	raw, err = os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image harness.ImageMetadata
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	tmp := image.Env["CLAUDE_CODE_TMPDIR"]
	if tmp == "" {
		t.Fatal("image env does not set CLAUDE_CODE_TMPDIR")
	}
	var persisted bool
	for _, volume := range image.Volumes {
		if volume.Path == tmp && volume.Volume == harness.VolumeData && !volume.ExcludeFromExport {
			persisted = true
		}
	}
	if !persisted {
		t.Errorf("CLAUDE_CODE_TMPDIR %q is not a data volume that travels on export: %#v", tmp, image.Volumes)
	}

	launcher, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, launchOnly := range []string{"CLAUDE_CODE_TMPDIR", "--settings"} {
		if strings.Contains(string(launcher), launchOnly) {
			t.Errorf("launch.sh sets %s, which a plain `claude` would not get", launchOnly)
		}
	}
}

// A discobox's terminal draws OSC 8 links (termpane keeps them), and the
// console's exec is given TERM=xterm-256color and nothing that says so. Claude
// Code does not read that as a terminal that takes hyperlinks, so it prints a
// markdown link as plain text. FORCE_HYPERLINK is the variable it reads first.
// It is the image's env rather than the launcher's, so a `claude` typed into a
// shell links too.
func TestImageSaysTheTerminalTakesHyperlinks(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image harness.ImageMetadata
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if got := image.Env["FORCE_HYPERLINK"]; got != "1" {
		t.Errorf("image env FORCE_HYPERLINK = %q, want \"1\": Claude Code does not write OSC 8 links for TERM alone", got)
	}
	launcher, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(launcher), "FORCE_HYPERLINK") {
		t.Error("launch.sh sets FORCE_HYPERLINK, which a plain `claude` would not get")
	}
}

// The list is Claude Code's own hook lifecycle table, which is published with
// the docs and readable as Markdown at
// https://docs.claude.com/en/docs/claude-code/hooks.md. Last checked against
// 2.1.278, which defines 33 events. A CLI that grows one is drift, not a
// failure of this image: the store hands a sandbox the newest version it has
// (ADR 0114), so the settings file is only complete for as long as nobody
// adds an event. Re-read that table when this test fails.
func TestManagedSettingsPublishesEverySupportedEvent(t *testing.T) {
	data, err := os.ReadFile("managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse managed settings: %v", err)
	}
	wantEvents := []string{
		"SessionStart", "Setup", "InstructionsLoaded", "UserPromptSubmit", "UserPromptExpansion",
		"MessageDisplay", "PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure",
		"PostToolBatch", "PermissionDenied", "Notification", "SubagentStart", "SubagentStop",
		"TaskCreated", "TaskCompleted", "Stop", "StopFailure", "TeammateIdle", "ConfigChange",
		"CwdChanged", "DirectoryAdded", "FileChanged", "WorktreeCreate", "WorktreeRemove",
		"PreCompact", "PostCompact", "PreModelSwitch", "PostModelSwitch", "SessionEnd",
		"Elicitation", "ElicitationResult",
	}
	if len(settings.Hooks) != len(wantEvents) {
		t.Fatalf("events = %d, want %d", len(settings.Hooks), len(wantEvents))
	}
	for _, event := range wantEvents {
		groups := settings.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("event %s = %#v, want one command hook", event, groups)
		}
		hook := groups[0].Hooks[0]
		wantCommand := "discobox-hook-publish --provider claude-code --event " + event
		if hook.Type != "command" || hook.Command != wantCommand || hook.Timeout <= 0 {
			t.Fatalf("event %s hook = %#v, want command %q with timeout", event, hook, wantCommand)
		}
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3): the command is typed, so the login shell splits the
// prompt before the launcher sees it, and the launcher hands `claude` the one
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
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "claude", nil, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("claude argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestLaunchKeysMemoriesByUID runs the launcher against a source-data mount:
// memories land in the running uid's partition, so sandboxes on one source
// share them only when they agree on a uid, and memories written before the
// partition existed are carried over to the uid that owns them. The path is
// recorded as a managed-settings drop-in rather than passed to this one
// `claude`, so every later `claude` in the sandbox gets it too.
func TestLaunchKeysMemoriesByUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the partition is named by a POSIX uid, and os.Getuid is -1 on Windows")
	}
	sourceData := t.TempDir()
	dropIns := filepath.Join(t.TempDir(), "managed-settings.d")
	paths := map[string]string{
		launchertest.SourceDataPath:           sourceData,
		"/etc/claude-code/managed-settings.d": dropIns,
	}
	legacy := filepath.Join(sourceData, "harnesses", "claude-code", "memories")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "MEMORY.md"), []byte("- remembered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	partition := filepath.Join(sourceData, "users", strconv.Itoa(os.Getuid()))
	memories := filepath.Join(partition, "harnesses", "claude-code", "memories")

	if got, want := launchertest.RunLauncher(t, "claude", paths, []string{"hi"}), []string{"hi"}; !slices.Equal(got, want) {
		t.Fatalf("claude argv = %#v, want %#v", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(dropIns, "discobox-memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		AutoMemoryDirectory string `json:"autoMemoryDirectory"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("drop-in %q: %v", raw, err)
	}
	if settings.AutoMemoryDirectory != memories {
		t.Fatalf("drop-in autoMemoryDirectory = %q, want %q", settings.AutoMemoryDirectory, memories)
	}
	carried, err := os.ReadFile(filepath.Join(memories, "MEMORY.md"))
	if err != nil || string(carried) != "- remembered\n" {
		t.Fatalf("legacy memories were not carried over: %q, %v", carried, err)
	}
	info, err := os.Stat(partition)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("uid partition mode = %v, want 0700", perm)
	}
	// A later launch finds the partition and does not carry the legacy tree
	// over a second time on top of what it has since written.
	if err := os.WriteFile(filepath.Join(legacy, "MEMORY.md"), []byte("- stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launchertest.RunLauncher(t, "claude", paths, nil)
	if carried, _ := os.ReadFile(filepath.Join(memories, "MEMORY.md")); string(carried) != "- remembered\n" {
		t.Fatalf("second launch rewrote the partition: %q", carried)
	}
}

// Claude Code's vocabulary is the canonical one (ADR 0146 §1), so every event
// this image publishes is its own canonical name. The mapping never rewrites
// one, and this is what says so.
func TestEveryPublishedEventIsItsOwnCanonicalName(t *testing.T) {
	raw, err := os.ReadFile("managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Hooks) == 0 {
		t.Fatal("managed settings publish no hooks")
	}
	for event := range settings.Hooks {
		if got := harness.CanonicalHookEvent(Driver{}.ID(), event); got != event {
			t.Errorf("CanonicalHookEvent(%s, %s) = %q, want %q", Driver{}.ID(), event, got, event)
		}
	}
}

// The image's settings open Claude Code in bypass mode, and a .claude.json that
// has not seen Claude Code's offer to make auto mode the default opens on that
// offer instead, whose first choice switches bypass off. The template marks it
// seen, so a sandbox starts in the mode its settings say.
func TestLauncherInitializesLocalState(t *testing.T) {
	raw, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "claude-code-capture init") {
		t.Fatal("launcher does not initialize local trust and onboarding")
	}
}

// Anthropic's API takes one login at many endpoints, and only inference answers
// for whether it is alive: the self-hosted runner poll refuses a subscription
// login that works, and event logging accepts whatever it is sent. Both
// credentials name /v1/messages, so neither of those draws or clears the
// sign-in band (ADR 26-10-13-595).
func TestImageScopesRejectionsToInference(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image harness.ImageMetadata
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if image.Harness == nil || len(image.Harness.Secrets) != 2 {
		t.Fatalf("manifest secrets = %#v, want the API key and the subscription login", image.Harness)
	}
	for _, secret := range image.Harness.Secrets {
		scope := secret.RejectionScope
		if len(scope) != 1 || scope[0].Host != "api.anthropic.com" || !slices.Equal(scope[0].Paths, []string{"/v1/messages"}) {
			t.Errorf("%s rejectionScope = %#v, want api.anthropic.com /v1/messages", secret.Name, scope)
		}
		for _, rule := range scope {
			if err := rule.Validate(); err != nil {
				t.Errorf("%s: %v", secret.Name, err)
			}
		}
	}
}

func TestLaunchInitializerPathsAreLiteral(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source with spaces ' and $dollars")
	work := filepath.Join(root, "harness", "claude-code")
	capture := filepath.Join(root, "harness", "capture")
	for _, dir := range []string{work, capture} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []struct{ source, target string }{
		{"launch.sh", filepath.Join(work, "launch.sh")},
		{"image.json", filepath.Join(work, "image.json")},
		{"../capture/runtime.cjs", filepath.Join(capture, "runtime.cjs")},
		{"../capture/settings.cjs", filepath.Join(capture, "settings.cjs")},
	} {
		data, err := os.ReadFile(file.source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)
	got := launchertest.RunLauncher(t, "claude", nil, []string{"fixture prompt"})
	if !slices.Equal(got, []string{"fixture prompt"}) {
		t.Fatalf("launcher argv = %#v", got)
	}
}
