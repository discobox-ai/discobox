package pi

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/featuretest"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

// The paths the image's pieces agree on, and only these tests hold them to it.
const (
	settingsPath     = ".config/discobox/pi-harness.json"
	authPath         = ".pi/agent/auth.json"
	piSettingsPath   = ".pi/agent/settings.json"
	hookShimPath     = ".pi/agent/extensions/discobox-hooks.js"
	installedHookExt = "/usr/local/libexec/discobox/pi-hook-extension.js"
)

type imageManifest struct {
	Env     map[string]string `json:"env"`
	Harness struct {
		ID              string         `json:"id"`
		Files           []harness.File `json:"files"`
		Secrets         []any          `json:"secrets"`
		RunCommand      []string       `json:"runCommand"`
		RelaunchCommand []string       `json:"relaunchCommand"`
		Config          struct {
			Ports   []harness.ConfigPort `json:"ports"`
			Command []string             `json:"command"`
		} `json:"config"`
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // A stub the script under test executes.
		t.Fatal(err)
	}
}

func TestDefinitionConfigure(t *testing.T) {
	script := readFile(t, "configure.sh") + readFile(t, "pi-normalize.cjs") + readFile(t, "image.json")
	for _, required := range []string{
		harness.ConfigureOutputPath,
		harness.ConfigurePreviousConfigPath,
		// Reuse goes through the PREV_ sentinel and comes back as usePrevious,
		// so no credential is read or re-emitted when the existing one is kept.
		`sentinels[envName]`,
		"usePrevious",
		// auth.json is JSON, so a quote inside a template action arrives at
		// the renderer backslash-escaped and the parser rejects it.
		"{{ .secrets.${envName} }}",
		// Every retry is a person asking for one.
		"confirm_retry",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("configure script is missing %q", required)
		}
	}
	if strings.Contains(script, "index .secrets") {
		t.Fatal("configure script quotes a key inside a template action, which cannot survive JSON encoding")
	}
	loopStart := strings.Index(script, "while :; do")
	if loopStart < 0 {
		t.Fatal("configure script has no configuration loop")
	}
	loop := script[loopStart:]
	if loopEnd := strings.Index(loop, "\ndone\n"); loopEnd >= 0 {
		loop = loop[:loopEnd]
	}
	// Split on `continue`: every segment but the last is the code leading up to
	// one, and each has to have asked before taking it. An attempt that fails
	// without reaching the user fails again the instant it is retried.
	segments := strings.Split(loop, "continue")
	for _, leadingUpToAContinue := range segments[:len(segments)-1] {
		if !strings.Contains(leadingUpToAContinue, "confirm_retry") {
			t.Fatal("configure script's loop continues without confirm_retry, so it can spin")
		}
	}
}

// TestImageManifest pins the half of the contract the image owns
// (ADR 26-10-15-259). Which secrets exist is only known once a user signs in,
// so the manifest declares none, and no baseline auth.json: one would land in
// every sandbox describing credentials with nothing behind them.
func TestImageManifest(t *testing.T) {
	image := readImage(t)
	if want := (Driver{}).Definition().ID; image.Harness.ID != want {
		t.Fatalf("manifest id = %q, want the definition's %q", image.Harness.ID, want)
	}
	if len(image.Harness.Secrets) != 0 {
		t.Fatalf("manifest declares secrets %v, want none", image.Harness.Secrets)
	}
	files := map[string]harness.File{}
	for _, file := range image.Harness.Files {
		// Both are rewritten on every launch rather than kept from the first,
		// or an image's new extension, or an edit to the settings, would never
		// land; neither holds a sentinel.
		if file.CreateOnly || file.Template {
			t.Errorf("file %s is createOnly or templated, want plain", file.Path)
		}
		files[file.Path] = file
	}
	if len(files) != 2 {
		t.Fatalf("manifest files = %v, want the hook extension and the settings", image.Harness.Files)
	}
	if _, ok := files[authPath]; ok {
		t.Fatal("manifest declares a baseline auth.json")
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(files[settingsPath].Content), &settings); err != nil {
		t.Fatalf("baseline settings are not JSON: %v", err)
	}
	if settings["judgeModel"] != "" {
		t.Fatalf("baseline settings = %v, want the default judge", settings)
	}
	// The manifest names no command: the launcher is installed under the
	// conventional name and the runtime types that (ADR 0086 §3).
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	// Browser sign-ins end at localhost ports, which only a forward from the
	// user's machine can reach — Anthropic's, OpenAI's and Radius's fixed, and
	// OpenRouter's picked fresh each run. Each says what to do when it cannot
	// be forwarded.
	var ports []int
	ephemeral := 0
	for _, port := range image.Harness.Config.Ports {
		if port.Ephemeral {
			ephemeral++
		} else {
			ports = append(ports, port.Port)
		}
		if port.Unavailable == "" {
			t.Errorf("port %d says nothing when it cannot be forwarded", port.Port)
		}
	}
	if !slices.Equal(ports, []int{53692, 1455, 1456}) || ephemeral != 1 {
		t.Fatalf("config ports = %v (+%d ephemeral)", ports, ephemeral)
	}
}

// The hook extension is reached through a file in the home that re-exports the
// one the image installs, so the code a sandbox runs is the image's
// (ADR 26-10-15-259 §3).
func TestTheHomeFileLoadsTheInstalledExtension(t *testing.T) {
	image := readImage(t)
	var shim string
	for _, file := range image.Harness.Files {
		if file.Path == hookShimPath {
			shim = file.Content
		}
	}
	if !strings.Contains(shim, `export { default } from "`+installedHookExt+`";`) {
		t.Fatalf("%s = %q, want it to re-export %s", hookShimPath, shim, installedHookExt)
	}
	if got := featuretest.Load(t, ".").Installs("hook-extension.js"); got != installedHookExt {
		t.Errorf("the feature installs the extension at %q, want %s", got, installedHookExt)
	}
}

// The policy baseline (ADR 26-10-15-259 §4): pi has no system layer, so it is
// the image's env and a launch flag — telemetry off, which pi otherwise sends
// on a first run and after each upgrade — and fd on PATH, which pi otherwise
// downloads from GitHub into the home on its first interactive run.
func TestImagePolicy(t *testing.T) {
	image := readImage(t)
	for key, want := range map[string]string{"PI_SKIP_VERSION_CHECK": "1", "PI_TELEMETRY": "0", "PI_CLIPBOARD_OSC52": "1"} {
		if image.Env[key] != want {
			t.Errorf("%s = %q, want %q", key, image.Env[key], want)
		}
	}
	// pi honors HTTPS_PROXY for every provider; omp's PI_PROXY workaround
	// has no counterpart here, and a proxy named in the manifest would drift
	// from the pool's.
	if v, ok := image.Env["PI_PROXY"]; ok {
		t.Errorf("manifest sets PI_PROXY=%q, which pi does not read", v)
	}
	feature := featuretest.Load(t, ".")
	for source, want := range map[string]string{
		"launch.sh":    "/usr/local/bin/" + harness.RunCommand,
		"prompt.sh":    "/usr/local/bin/discobox-prompt",
		"configure.sh": image.Harness.Config.Command[0],
		"agent.conf":   "/usr/local/libexec/discobox/agent.conf",
	} {
		if got := feature.Installs(source); got != want {
			t.Errorf("the feature installs %s at %q, want %q", source, got, want)
		}
	}
	if !feature.InstallsPackage("fd-find") {
		t.Error("the feature does not install fd-find")
	}
	if !feature.Runs(t, "npm install -g @earendil-works/pi-coding-agent") {
		t.Error("the feature does not install pi's CLI")
	}
	conf := readFile(t, "agent.conf")
	if !strings.Contains(conf, "AGENT_PACKAGE=@earendil-works/pi-coding-agent\n") || !strings.Contains(conf, "AGENT_BIN=pi\n") {
		t.Error("agent.conf does not name pi's package and binary")
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). pi sends each positional argument as a message of
// its own, so the joined prompt is one argument after `--`; and it reads an
// @-led argument as a file to attach even after `--`, so such a prompt leads
// with a space.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	policy := []string{"--approve"}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"--", "fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"--", "fix the failing tests"}},
		{"a dash-led prompt stays a prompt", []string{"-v", "explain this flag"}, []string{"--", "-v explain this flag"}},
		{"an @-led prompt is not a file", []string{"@main.go", "explain"}, []string{"--", " @main.go explain"}},
		{"an @ later on is left alone", []string{"explain", "@main.go"}, []string{"--", "explain @main.go"}},
		{"no prompt passes none", nil, nil},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "pi", nil, tc.args)
			if want := append(slices.Clone(policy), tc.want...); !slices.Equal(got, want) {
				t.Fatalf("pi argv = %#v, want %#v", got, want)
			}
		})
	}
}
