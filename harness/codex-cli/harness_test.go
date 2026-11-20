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
	"github.com/discobox-ai/discobox/harness/internal/harnesstest"
	"github.com/discobox-ai/discobox/internal/harnessbundle"
)

// codexBox installs the staged harness with an argv-printing codex. Every
// launch trusts its folder through the runtime, so it needs node.
func codexBox(t *testing.T, paths map[string]string) *harnesstest.Box {
	t.Helper()
	harnesstest.RequireNode(t)
	box := harnesstest.Stage(t, ".").Install(t, paths)
	box.Stub(t, "codex", harnesstest.ArgvStub)
	return box
}

// The list is Codex's own hook lifecycle table at
// https://developers.openai.com/codex/hooks (the repository ships no
// equivalent Markdown). Last checked against 0.155.1, which defines 12
// events. Interrupt is the one that does not fire for subagents, and
// SessionEnd is the one that fires only for the main thread. See the
// claude-code twin of this test for why the list goes stale.
func TestImageOwnedHooksPublishEveryCodexLifecycleEvent(t *testing.T) {
	raw := harnesstest.Stage(t, ".").Installed(t, "/etc/codex/hooks.json")
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
	raw, err := os.ReadFile("assets/system-config.toml")
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
	raw := harnesstest.Stage(t, ".").Label()
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
	staged := harnesstest.Stage(t, ".")
	var image struct {
		Harness struct {
			RunCommand      []string `json:"runCommand"`
			RelaunchCommand []string `json:"relaunchCommand"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(staged.Label(), &image); err != nil {
		t.Fatal(err)
	}
	// The manifest names no command: the launcher is installed under the
	// conventional name and the runtime types that (ADR 0086 §3). Declaring one
	// here would be an override, and would silently outrank the convention.
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	if got, want := staged.Installs(harnessbundle.LauncherSource), "/usr/local/bin/"+harness.RunCommand; got != want {
		t.Fatalf("the feature installs the launcher at %q, want %q", got, want)
	}
	script := string(staged.FeatureFile(t, harnessbundle.LauncherSource))
	for _, required := range []string{
		// The resume half of the convention, and the prompt that trails it on
		// every launch (ADR 0086 §4), are TestLaunchJoinsThePromptWords's.
		"--resume 'resume --last'",
		// Codex sends a copy to the user's terminal only in an SSH session;
		// without it every copy lands on the box's own X clipboard.
		"discobox_harness_env_default 'SSH_CONNECTION'",
		// A bind mount, never a symlink: Codex rejects a symlinked memory
		// root.
		"discobox_harness_memory_bind_mount 'Codex' 'codex' 'CODEX_HOME' '.codex' 'memories'",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("launcher is missing %q:\n%s", required, script)
		}
	}
	config, err := os.ReadFile("assets/system-config.toml")
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

// Codex copies to its terminal only in what it takes for an SSH session; a
// real session's value is kept.
func TestLaunchSaysTheSessionIsRemote(t *testing.T) {
	box := codexBox(t, nil)
	box.Stub(t, "codex", "#!/bin/sh\nprintf '%s\\n' \"$SSH_CONNECTION\"\n")
	if got := box.Argv(t, "hi"); !slices.Equal(got, []string{"127.0.0.1 0 127.0.0.1 0"}) {
		t.Fatalf("SSH_CONNECTION = %q", got)
	}
	box.Env("SSH_CONNECTION=10.0.0.1 22 10.0.0.2 2222")
	if got := box.Argv(t, "hi"); !slices.Equal(got, []string{"10.0.0.1 22 10.0.0.2 2222"}) {
		t.Fatalf("SSH_CONNECTION = %q, want the real session's", got)
	}
}

// The system layer trusts each folder a launch starts in, beside every folder
// an earlier launch trusted, so a later direct `codex` start sees it too; it
// keeps the baseline's settings under the projects.
func TestLaunchTrustsTheFolderInTheSystemLayer(t *testing.T) {
	box := codexBox(t, nil)
	box.Argv(t, "hi")
	other := filepath.Join(box.Home, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := box.Command(t, "/usr/local/bin/discobox-harness-run", "hi")
	cmd.Dir = other
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	layer, err := os.ReadFile(box.Path("/etc/codex/config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(box.Path("/usr/local/libexec/discobox/codex-system-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := filepath.EvalSymlinks(box.Home)
	other, _ = filepath.EvalSymlinks(other)
	for _, folder := range []string{home, other} {
		if !strings.Contains(string(layer), strconv.Quote(folder)+`={trust_level="trusted"}`) {
			t.Errorf("system layer does not trust %s:\n%s", folder, layer)
		}
	}
	if !strings.HasPrefix(string(layer), "projects={") || !strings.HasSuffix(string(layer), string(baseline)) {
		t.Errorf("system layer is not its projects then the baseline:\n%s", layer)
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
			got := codexBox(t, nil).Argv(t, tc.args...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("codex argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestLaunchRetriesAStartThatFailedBeforeItsSession covers issue #767: Codex
// exits with status 1 at TUI bootstrap ("account/read failed ... workspace
// routing discovery failed") when its first request out cannot connect, and a
// box's first request can meet a transient failure. The launcher starts Codex
// again then, and only then: a session that began (it wrote a rollout) would
// get its prompt a second time, a clean exit is how the user quits, and a
// signal is the user or the terminal ending it, and any other status would
// fail the same way again.
func TestLaunchRetriesAStartThatFailedBeforeItsSession(t *testing.T) {
	// The stub fails with $CODEX_TEST_STATUS for its first $CODEX_TEST_FAILS
	// runs and succeeds after. Before failing it starts a session the way
	// $CODEX_TEST_SESSION says: "new" writes a rollout, "resume" appends to
	// the one already there. It counts its runs under $HOME and reports each
	// on stdout, as the sleep stub reports each wait instead of waiting.
	const codex = `#!/bin/sh
printf 'x\n' >>"$HOME/runs"
runs=$(wc -l <"$HOME/runs")
printf 'codex %s\n' "$*"
if [ "$runs" -le "$CODEX_TEST_FAILS" ]; then
	case "$CODEX_TEST_SESSION" in
	new) : >"$CODEX_HOME/sessions/rollout-$runs.jsonl" ;;
	resume) printf '{}\n' >>"$CODEX_HOME/sessions/rollout-0.jsonl" ;;
	esac
	exit "$CODEX_TEST_STATUS"
fi
`
	const sleep = "#!/bin/sh\nprintf 'sleep %s\\n' \"$1\"\n"
	const run = "codex fix the tests"
	for _, tc := range []struct {
		name       string
		args       []string
		fails      int
		status     int
		session    string
		wantStatus int
		want       []string
	}{
		{"a bootstrap failure is started again", nil, 2, 1, "", 0,
			[]string{run, "sleep 2", run, "sleep 5", run}},
		{"a start that keeps failing gives up after five", nil, 9, 1, "", 1,
			[]string{run, "sleep 2", run, "sleep 5", run, "sleep 10", run, "sleep 20", run}},
		{"a clean exit is not retried", nil, 0, 0, "", 0, []string{run}},
		{"a session that began is not started again", nil, 1, 1, "new", 1, []string{run}},
		{"a death by signal is not retried", nil, 1, 130, "", 130, []string{run}},
		{"a failure that would repeat is not retried", nil, 1, 127, "", 127, []string{run}},
		{"a resume that failed to start is resumed again", []string{harness.ResumeFlag, "fix", "the", "tests"}, 1, 1, "", 0,
			[]string{"codex resume --last", "sleep 2", "codex resume --last"}},
		{"a resumed session is not resumed again", []string{harness.ResumeFlag, "fix", "the", "tests"}, 1, 1, "resume", 1,
			[]string{"codex resume --last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A session from an earlier launch is already there, as it is for
			// a resume.
			codexHome := t.TempDir()
			if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(codexHome, "sessions", "rollout-0.jsonl"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			box := codexBox(t, nil)
			box.Stub(t, "codex", codex)
			box.Stub(t, "sleep", sleep)
			box.Env("CODEX_HOME="+codexHome, "CODEX_TEST_FAILS="+strconv.Itoa(tc.fails),
				"CODEX_TEST_STATUS="+strconv.Itoa(tc.status), "CODEX_TEST_SESSION="+tc.session)
			args := tc.args
			if args == nil {
				args = []string{"fix", "the", "tests"}
			}
			out, status := box.Launch(t, args...)
			if status != tc.wantStatus {
				t.Errorf("launcher exit status = %d, want %d", status, tc.wantStatus)
			}
			if got := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); !slices.Equal(got, tc.want) {
				t.Errorf("launcher ran %#v, want %#v", got, tc.want)
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

	if got, want := codexBox(t, map[string]string{harnesstest.SourceDataPath: sourceData}).Argv(t, "hi"), []string{"hi"}; !slices.Equal(got, want) {
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
	staged := harnesstest.Stage(t, ".")
	raw := staged.Installed(t, "/etc/codex/hooks.json")
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
		canonical := harness.CanonicalHookEvent(staged.Declaration().Hooks.Provider, event)
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
