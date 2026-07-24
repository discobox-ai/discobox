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
	// The subscription path signs in with /login and captures the rotating OAuth
	// blob (refresh token included) as an oauth secret, not the non-rotating
	// `claude setup-token`, so the control plane can refresh the access token.
	if !strings.Contains(script, "/login") {
		t.Fatalf("configure script offers no subscription login: %s", script)
	}
	if !strings.Contains(script, "refreshToken") || !strings.Contains(script, "'oauth'") {
		t.Fatalf("configure script does not capture a rotating oauth credential: %s", script)
	}
	// The configure sandbox has no source, so the workspace is not trusted by the
	// image template; the script must trust it itself or `claude /login` stops at
	// the trust dialog.
	if !strings.Contains(script, "hasTrustDialogAccepted") {
		t.Fatalf("configure script does not pre-trust the workspace for login: %s", script)
	}
	if !strings.Contains(script, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(script, "ANTHROPIC_API_KEY") {
		t.Fatalf("configure script does not offer both auth secrets: %s", script)
	}
	if !strings.Contains(script, "claude -p") {
		t.Fatalf("configure script does not verify the credential with a test prompt: %s", script)
	}
	// Credentials are secrets, never public harness files: the flow returns the
	// credential as a secret and leaves the non-secret files to the image.
	if strings.Contains(script, "files.push(") || strings.Contains(script, ".credentials.json'") {
		t.Fatalf("configure script exposes credential state as a public harness file: %s", script)
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
