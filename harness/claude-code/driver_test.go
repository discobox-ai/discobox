package claudecode

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/harness"
)

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

// hooksFromEvents builds ascending-by-time HookRecords from event names, one
// second apart, matching the order store.ListHarnessHooks already returns.
func hooksFromEvents(events ...string) []harness.HookRecord {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hooks := make([]harness.HookRecord, len(events))
	for i, event := range events {
		hooks[i] = harness.HookRecord{Event: event, CreatedAt: base.Add(time.Duration(i) * time.Second)}
	}
	return hooks
}

func TestDeriveSessionState(t *testing.T) {
	tests := []struct {
		name          string
		events        []string
		wantState     string
		wantLastEvent string
	}{
		{
			name:      "no hooks yet",
			events:    nil,
			wantState: "",
		},
		{
			name:          "session start is running",
			events:        []string{"SessionStart"},
			wantState:     harness.SessionStateRunning,
			wantLastEvent: "SessionStart",
		},
		{
			name:          "tool use keeps running",
			events:        []string{"SessionStart", "UserPromptSubmit", "PreToolUse"},
			wantState:     harness.SessionStateRunning,
			wantLastEvent: "PreToolUse",
		},
		{
			name:          "stop is idle",
			events:        []string{"SessionStart", "UserPromptSubmit", "Stop"},
			wantState:     harness.SessionStateIdle,
			wantLastEvent: "Stop",
		},
		{
			name:          "notification after stop is needs_input",
			events:        []string{"SessionStart", "Stop", "PermissionRequest"},
			wantState:     harness.SessionStateNeedsInput,
			wantLastEvent: "PermissionRequest",
		},
		{
			name:          "informational event after stop does not overwrite idle",
			events:        []string{"SessionStart", "UserPromptSubmit", "Stop", "ConfigChange"},
			wantState:     harness.SessionStateIdle,
			wantLastEvent: "ConfigChange",
		},
		{
			name:          "informational event after needs_input does not overwrite it",
			events:        []string{"SessionStart", "PostToolUse", "Notification", "PermissionDenied"},
			wantState:     harness.SessionStateNeedsInput,
			wantLastEvent: "PermissionDenied",
		},
		{
			name:          "session end is idle",
			events:        []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"},
			wantState:     harness.SessionStateIdle,
			wantLastEvent: "SessionEnd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := hooksFromEvents(tt.events...)
			gotState, gotLastEvent, gotLastEventAt := Driver{}.DeriveSessionState(hooks)
			if gotState != tt.wantState {
				t.Errorf("state = %q, want %q", gotState, tt.wantState)
			}
			if gotLastEvent != tt.wantLastEvent {
				t.Errorf("lastEvent = %q, want %q", gotLastEvent, tt.wantLastEvent)
			}
			if len(hooks) > 0 && !gotLastEventAt.Equal(hooks[len(hooks)-1].CreatedAt) {
				t.Errorf("lastEventAt = %v, want %v", gotLastEventAt, hooks[len(hooks)-1].CreatedAt)
			}
		})
	}
}
