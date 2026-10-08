package copilot

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

func TestDefinitionConfigure(t *testing.T) {
	def := Driver{}.Definition()
	if def.Configure == nil {
		t.Fatal("Configure = nil, want a configure spec")
	}
	if id := (Driver{}).ID(); def.ID != id {
		t.Fatalf("definition ID %q differs from the driver's %q, which is also its hook provider", def.ID, id)
	}
	scriptBytes, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, want := range []string{
		harness.ConfigureOutputPath,
		harness.ConfigurePreviousConfigPath,
		// Reuse goes through the PREV_ sentinel and comes back as usePrevious,
		// so no credential is read or re-emitted when the existing one is kept.
		"'" + harness.ConfigurePreviousEnvPrefix + "' + envName",
		"usePrevious",
		// Every retry is gated on a person asking for one; see the loop check
		// below.
		"confirm_retry",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("configure script is missing %q", want)
		}
	}
	// Every `continue` in the collection loop has to have asked first. An
	// attempt that fails without reaching the user -- Copilot refusing to
	// start, say -- fails again the instant it is retried, so an ungated
	// `continue` is a busy loop rather than a retry.
	loopStart := strings.Index(script, "while [ -z \"$TOKEN\" ]; do")
	if loopStart < 0 {
		t.Fatal("configure script has no credential-collection loop")
	}
	loop := script[loopStart:]
	if loopEnd := strings.Index(loop, "\ndone\n"); loopEnd >= 0 {
		loop = loop[:loopEnd]
	}
	segments := strings.Split(loop, "continue")
	for _, leadingUpToAContinue := range segments[:len(segments)-1] {
		if !strings.Contains(leadingUpToAContinue, "confirm_retry") {
			t.Fatalf("configure script's loop continues without confirm_retry, so it can spin: %s", leadingUpToAContinue)
		}
	}
	// Emphasis is opt-out, not unconditional: this output is also read from a
	// log, and a hardcoded escape sequence would land there too.
	if strings.Contains(script, `\033[`) && !strings.Contains(script, "NO_COLOR") {
		t.Fatal("configure script colorizes without honoring NO_COLOR")
	}
}

// TestImageDeclaresOneEnvDeliveredToken pins the half of the contract the
// image owns (ADR 26-10-02-840 §1): one required token, in the variable Copilot
// documents for headless use, exported rather than rendered into a file.
func TestImageDeclaresOneEnvDeliveredToken(t *testing.T) {
	image := readImage(t)
	if len(image.Harness.Secrets) != 1 {
		t.Fatalf("secrets = %#v, want exactly COPILOT_GITHUB_TOKEN", image.Harness.Secrets)
	}
	secret := image.Harness.Secrets[0]
	if secret.Name != "COPILOT_GITHUB_TOKEN" || !secret.Required {
		t.Fatalf("secret = %#v, want a required COPILOT_GITHUB_TOKEN", secret)
	}
	if secret.Delivery != harness.SecretDeliveryEnv {
		t.Fatalf("secret delivery = %q, want the environment: Copilot reads the variable, and its stored-login file is undocumented state", secret.Delivery)
	}
	// Copilot's state file is where a stored login lives. A baseline one would
	// land in every sandbox and be rewritten under Copilot on each launch.
	for _, file := range image.Harness.Files {
		if file.Path == ".copilot/config.json" {
			t.Errorf("image declares %s, which is Copilot's own state", file.Path)
		}
	}
}

// TestImagePolicy pins the two switches the baseline is (ADR 26-10-02-840 §4):
// the env trusts the directory Copilot starts in, and the launcher's flag
// approves its tools. Either alone leaves an interactive session waiting on a
// person — a trust dialog, or manual approval.
func TestImagePolicy(t *testing.T) {
	image := readImage(t)
	if image.Env["COPILOT_ALLOW_ALL"] != "true" {
		t.Errorf("COPILOT_ALLOW_ALL = %q, want exactly \"true\": only that spelling also trusts the working directory", image.Env["COPILOT_ALLOW_ALL"])
	}
	// The version comes from the pool-cached store (ADR 0114), never from
	// Copilot updating itself.
	if image.Env["COPILOT_AUTO_UPDATE"] != "false" {
		t.Errorf("COPILOT_AUTO_UPDATE = %q, want \"false\"", image.Env["COPILOT_AUTO_UPDATE"])
	}
	// The manifest names no command: the launcher is installed under the
	// conventional name and the runtime types that (ADR 0086 §3).
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"launch.sh /usr/local/bin/" + harness.RunCommand,
		"prompt.sh /usr/local/bin/discobox-prompt",
		"hooks.json /etc/github-copilot/policy.d/",
		"agent.conf /usr/local/libexec/discobox/agent.conf",
	} {
		if !strings.Contains(string(dockerfile), want) {
			t.Errorf("Dockerfile does not install %q", want)
		}
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3): the command is typed, so the login shell splits the
// prompt before the launcher sees it, and the launcher hands Copilot the one
// prompt it submits in its TUI.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"--allow-all", "--interactive=fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"--allow-all", "--interactive=fix the failing tests"}},
		// The `=` form is what keeps this a prompt rather than a flag.
		{"a prompt that starts with a dash", []string{"-n", "is", "not", "a", "flag"}, []string{"--allow-all", "--interactive=-n is not a flag"}},
		{"no prompt passes none", nil, []string{"--allow-all"}},
		// A resumed session already carries the prompt (ADR 0086 §4), so the
		// launcher replaces it rather than sending it a second time.
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--allow-all", "--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "copilot", nil, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("copilot argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The list is every hook event Copilot 1.0.91's settings schema names
// (`hooks.<event>`). A Copilot release that adds one should be published, or
// listed here as deliberately not — so this goes stale on purpose.
func TestPolicyHooksPublishEveryCopilotLifecycleEvent(t *testing.T) {
	config := readHooks(t)
	if config.Version != 1 {
		t.Fatalf("hooks version = %d, want 1", config.Version)
	}
	wantEvents := []string{
		"sessionStart", "sessionEnd", "userPromptSubmitted", "userPromptTransformed",
		"preToolUse", "preMcpToolCall", "permissionRequest", "postToolUse",
		"postToolUseFailure", "errorOccurred", "agentStop", "subagentStart",
		"subagentStop", "preCompact", "notification",
	}
	if len(config.Hooks) != len(wantEvents) {
		t.Fatalf("events = %d, want %d: %#v", len(config.Hooks), len(wantEvents), config.Hooks)
	}
	for _, event := range wantEvents {
		hooks := config.Hooks[event]
		if len(hooks) != 1 {
			t.Fatalf("event %s does not have exactly one publisher: %#v", event, hooks)
		}
		hook := hooks[0]
		wantCommand := "discobox-hook-publish --provider " + Driver{}.ID() + " --event " + event
		if hook.Type != "command" || hook.Bash != wantCommand {
			t.Errorf("event %s hook = %#v", event, hook)
		}
		if hook.TimeoutSec <= 0 || hook.TimeoutSec > 3 {
			t.Errorf("event %s timeout = %d, want 1..3 seconds", event, hook.TimeoutSec)
		}
	}
}

// The mapping table and this image's hook config are one thing in two files,
// and nothing but this test keeps them together. Every event the image
// publishes must either have a canonical name or be listed here as having
// none (ADR 0146 §3).
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	config := readHooks(t)
	// Copilot events Claude Code has no word for. userPromptTransformed is the
	// model-facing form of a prompt and preMcpToolCall an MCP request about to
	// leave, neither of which Claude Code hooks. errorOccurred fires when a
	// model call fails, which Copilot may retry, so it is not StopFailure: a
	// wait on that would end while the turn goes on.
	noCanonicalName := []string{"userPromptTransformed", "preMcpToolCall", "errorOccurred"}

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

type imageManifest struct {
	Env     map[string]string `json:"env"`
	Harness struct {
		RunCommand      []string       `json:"runCommand"`
		RelaunchCommand []string       `json:"relaunchCommand"`
		Files           []harness.File `json:"files"`
		Secrets         []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
			Delivery string `json:"delivery"`
		} `json:"secrets"`
	} `json:"harness"`
}

func readImage(t *testing.T) imageManifest {
	t.Helper()
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image imageManifest
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	return image
}

type hooksConfig struct {
	Version int `json:"version"`
	Hooks   map[string][]struct {
		Type       string `json:"type"`
		Bash       string `json:"bash"`
		TimeoutSec int    `json:"timeoutSec"`
	} `json:"hooks"`
}

func readHooks(t *testing.T) hooksConfig {
	t.Helper()
	raw, err := os.ReadFile("hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var config hooksConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}
