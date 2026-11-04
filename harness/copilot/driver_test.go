package copilot

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/featuretest"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

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
	feature := featuretest.Load(t, ".")
	for source, want := range map[string]string{
		"launch.sh":  "/usr/local/bin/" + harness.RunCommand,
		"agent.conf": "/usr/local/libexec/discobox/agent.conf",
	} {
		if got := feature.Installs(source); got != want {
			t.Errorf("the feature installs %s at %q, want %q", source, got, want)
		}
	}
	if got := feature.Installs("hooks.json"); !strings.HasPrefix(got, "/etc/github-copilot/policy.d/") {
		t.Errorf("the feature installs hooks.json at %q, not in Copilot's policy.d", got)
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
			Name     string                 `json:"name"`
			Required bool                   `json:"required"`
			Delivery string                 `json:"delivery"`
			Routes   []harnesscapture.Route `json:"routes"`
		} `json:"secrets"`
	} `json:"harness"`
}

// The token's routes send Copilot's own traffic, refuse its MCP and agent
// calls and GitHub beyond Copilot — the routes a judge used to read, blocked
// so removing it widened nothing (ADR 26-10-27-880) — and send it nowhere
// else (ADR 26-10-02-924).
func TestTheTokensRoutesSendCopilotsOwnTrafficAndBlockTheRest(t *testing.T) {
	image := readImage(t)
	var routes []secretroute.Route
	for _, secret := range image.Harness.Secrets {
		if secret.Name != "COPILOT_GITHUB_TOKEN" {
			continue
		}
		for i, route := range secret.Routes {
			normalized, err := secretroute.Normalize(secretroute.Route(route))
			if err != nil {
				t.Fatalf("route %d: %v", i, err)
			}
			if !reflect.DeepEqual(normalized, secretroute.Route(route)) {
				t.Fatalf("route %d = %+v, want it written as it normalizes, %+v", i, route, normalized)
			}
			routes = append(routes, normalized)
		}
	}
	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"https://api.individual.githubcopilot.com/v1/messages", true},
		{"https://api.individual.githubcopilot.com/chat/completions", true},
		{"https://telemetry.individual.githubcopilot.com/telemetry", true},
		{"https://api.individual.githubcopilot.com/mcp/readonly", true},
		{"https://api.individual.githubcopilot.com/agents/swe/internal/memory/x", true},
		{"https://api.github.com/copilot_internal/user", true},
		{"https://api.github.com/repos/github/copilot-cli/releases/latest", true},
		{"https://api.individual.githubcopilot.com/mcp", false},
		{"https://api.individual.githubcopilot.com/agents/swe/v1/jobs", false},
		{"https://api.github.com/repos/org/repo/pulls", false},
		{"https://github.com/org/repo", false},
	} {
		host := strings.SplitN(strings.TrimPrefix(tc.url, "https://"), "/", 2)[0]
		request := secretroute.Request{Method: "POST", Host: host, URL: tc.url}
		if allowed, reason := secretroute.Decide([][]secretroute.Route{routes}, request); allowed != tc.allowed {
			t.Fatalf("%s: allowed = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
	}
	if secretroute.HostAllowed([][]secretroute.Route{routes}, "example.com") {
		t.Fatal("example.com: allowed, want no route to it")
	}
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
