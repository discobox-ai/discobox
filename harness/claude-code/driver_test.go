package claudecode

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
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
		"/.discobox/data-per-source/primary",
		"harnesses/claude-code/memories",
		"autoMemoryDirectory",
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
	// The configure sandbox has no source, so the workspace is not trusted by
	// the image template; the script must trust it itself or the interactive
	// session stops at the trust dialog.
	if !strings.Contains(script, "hasTrustDialogAccepted") {
		t.Fatalf("configure script does not pre-trust the workspace for login: %s", script)
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
		"PreCompact", "PostCompact", "SessionEnd", "Elicitation", "ElicitationResult",
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
