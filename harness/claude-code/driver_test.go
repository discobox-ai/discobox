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
			Config          struct {
				Reminder string `json:"reminder"`
			} `json:"config"`
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
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if want := "launch.sh /usr/local/bin/" + harness.RunCommand; !strings.Contains(string(dockerfile), want) {
		t.Fatalf("Dockerfile does not install the launcher as %q", want)
	}
	for _, command := range []string{"/login", "/model", "/config", "/exit"} {
		if !strings.Contains(image.Harness.Config.Reminder, command) {
			t.Fatalf("configure reminder %q is missing %s", image.Harness.Config.Reminder, command)
		}
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

func TestDefinitionConfigure(t *testing.T) {
	def := Driver{}.Definition()
	if def.Configure == nil {
		t.Fatal("Configure = nil, want a configure spec")
	}
	scriptBytes, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	if !strings.Contains(script, harness.ConfigureOutputPath) {
		t.Fatalf("configure script does not write the harness-configure.json contract: %s", script)
	}
	if !strings.Contains(script, harness.ConfigurePreviousConfigPath) {
		t.Fatalf("configure script ignores the seeded previous configuration: %s", script)
	}
	// Reuse goes through the PREV_ sentinel and comes back as usePrevious, so no
	// credential is read or re-emitted when the existing one is kept.
	if !strings.Contains(script, harness.ConfigurePreviousEnvPrefix+"$PREVIOUS_ENV") {
		t.Fatalf("configure script does not read the PREV_ sentinel: %s", script)
	}
	if !strings.Contains(script, "usePrevious") {
		t.Fatalf("configure script cannot report keeping the previous secret: %s", script)
	}
	// A subscription login (chosen from inside the interactive session, not
	// driven by this script) captures the rotating OAuth blob (refresh token
	// included) as an oauth secret, not the non-rotating `claude setup-token`,
	// so the control plane can refresh the access token.
	if !strings.Contains(script, "/login") {
		t.Fatalf("configure script does not document a subscription login: %s", script)
	}
	if !strings.Contains(script, "refreshToken") || !strings.Contains(script, "'oauth'") {
		t.Fatalf("configure script does not capture a rotating oauth credential: %s", script)
	}
	// The scopes the login actually carries are only knowable here, and a client
	// gates features on the ones recorded beside the token, so they are copied
	// out with it rather than assumed later.
	if !strings.Contains(script, "oauth.scopes") {
		t.Fatalf("configure script does not capture the login's scopes: %s", script)
	}
	// The credentials template is JSON-encoded to become the file's content, so
	// a quote inside a template action arrives at the renderer backslash-escaped
	// and the parser rejects it. Dotted field access has no quotes to escape;
	// `index .secrets "NAME"` does, and would break every sandbox launch while
	// still letting configure report success.
	if !strings.Contains(script, "{{ .secrets.${") {
		t.Fatalf("configure script does not build the credentials template by field access: %s", script)
	}
	if strings.Contains(script, `index .secrets`) {
		t.Fatalf("configure script quotes a key inside a template action, which cannot survive JSON encoding: %s", script)
	}
	// An Anthropic Console account login is the other credential shape claude's
	// own onboarding offers; its long-lived managed key lands in primaryApiKey.
	if !strings.Contains(script, "primaryApiKey") {
		t.Fatalf("configure script does not capture a console-managed API key: %s", script)
	}
	// Directory trust belongs to the image's .claude.json template, which trusts
	// the directory the sandbox's terminals start in whether or not the sandbox
	// has a source -- a configure sandbox has none. The script must not write a
	// second trust map of its own: it runs in a throwaway sandbox, so what it
	// writes there is a map nothing outside it ever sees.
	if strings.Contains(script, "hasTrustDialogAccepted") {
		t.Fatalf("configure script writes its own trust map instead of leaving it to the image template: %s", script)
	}
	if !strings.Contains(script, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(script, "ANTHROPIC_API_KEY") {
		t.Fatalf("configure script does not offer both auth secrets: %s", script)
	}
	if !strings.Contains(script, "claude -p") {
		t.Fatalf("configure script does not verify the credential with a test prompt: %s", script)
	}
	// The credential itself is a secret, never a public harness file: only the
	// settings snapshot is returned as a file, and only from SETTINGS_FILE, not
	// from the files that hold or derive from a credential.
	if !strings.Contains(script, "files.push(") || !strings.Contains(script, "CLAUDE_CONFIGURE_SETTINGS_PATH") {
		t.Fatalf("configure script does not capture the settings file: %s", script)
	}
	writeOutputStart := strings.Index(script, "write_output() {")
	if writeOutputStart < 0 {
		t.Fatal("configure script has no write_output function")
	}
	writeOutputBody := script[writeOutputStart:]
	writeOutputEnd := strings.Index(writeOutputBody, "\n}\n")
	if writeOutputEnd < 0 {
		t.Fatal("configure script's write_output function has no closing brace")
	}
	if strings.Contains(writeOutputBody[:writeOutputEnd], ".credentials.json") {
		t.Fatalf("configure script's output writer reads the credentials file directly: %s", script)
	}
	// Every retry is gated on a person asking for one. An attempt that fails
	// without ever reaching the user -- claude refusing to start, say -- fails
	// again the instant it is retried, so an ungated `continue` is a busy loop
	// rather than a retry.
	if !strings.Contains(script, "confirm_retry") {
		t.Fatalf("configure script retries without asking, so a failing launch spins: %s", script)
	}
	loopStart := strings.Index(script, "while [ -z \"$ENV_NAME\" ]; do")
	if loopStart < 0 {
		t.Fatal("configure script has no credential-collection loop")
	}
	loop := script[loopStart:]
	if loopEnd := strings.Index(loop, "\ndone\n"); loopEnd >= 0 {
		loop = loop[:loopEnd]
	}
	// Split on `continue`: every segment but the last is the code leading up to
	// one, and each has to have asked before taking it.
	segments := strings.Split(loop, "continue")
	for _, leadingUpToAContinue := range segments[:len(segments)-1] {
		if !strings.Contains(leadingUpToAContinue, "confirm_retry") {
			t.Fatalf("configure script's loop continues without confirm_retry, so it can spin: %s", script)
		}
	}
	// claude repaints the terminal on start, and is moving to a full-screen UI,
	// so the banner naming /login, /model and /config has to be acknowledged
	// before the launch or the user never sees it.
	if !strings.Contains(script, "confirm_launch") {
		t.Fatalf("configure script launches claude without holding its instructions on screen: %s", script)
	}
	// /login and /exit are the two steps setup cannot finish without, and a
	// user dropped into a familiar CLI will read it as a working session unless
	// told otherwise, so the banner names both alongside the optional ones.
	for _, command := range []string{"/login", "/exit", "/model", "/config"} {
		if !strings.Contains(script, command) {
			t.Fatalf("configure script does not point the user at %s: %s", command, script)
		}
	}
	// Emphasis is opt-out, not unconditional: this output is also read from a
	// log, and a hardcoded escape sequence would land there too.
	if strings.Contains(script, `\033[`) && !strings.Contains(script, "NO_COLOR") {
		t.Fatalf("configure script colorizes without honoring NO_COLOR: %s", script)
	}
	// Reconfigure opens the session already signed in, so it can be used to
	// change a setting without re-authenticating. The old keep-or-replace prompt
	// made that impossible: keeping never launched claude at all.
	if !strings.Contains(script, "seed_previous_credential") {
		t.Fatalf("configure script does not sign the reconfigure session in: %s", script)
	}
	if strings.Contains(script, "Keep the existing credential") {
		t.Fatalf("configure script still asks keep-or-replace up front: %s", script)
	}
	// What was seeded is a sentinel we chose, so finding it still in place is
	// what proves nothing re-authenticated -- that comparison is the whole
	// change check, and without it every reconfigure would store a credential.
	if !strings.Contains(script, `!= "$SEEDED_SENTINEL"`) {
		t.Fatalf("configure script does not detect auth changes by comparison: %s", script)
	}
	// The launch's exit status is what separates "you did not sign in" from
	// "claude would not run", and the retry prompt is useless if the script
	// cannot tell the user which happened.
	if !strings.Contains(script, "claude_status") {
		t.Fatalf("configure script ignores whether claude actually ran: %s", script)
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
