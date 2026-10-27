package omp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/featuretest"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

// The paths the image's pieces agree on, and only these tests hold them to it.
const (
	settingsPath     = ".config/discobox/omp-harness.json"
	credentialsPath  = ".config/discobox/omp-credentials.json" //nolint:gosec // A file path, not a credential.
	hookShimPath     = ".omp/agent/extensions/discobox-hooks.js"
	installedHookExt = "/usr/local/libexec/discobox/omp-hook-extension.js"
	overlayPath      = "/etc/omp/discobox.yml"
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

func TestDefinitionConfigure(t *testing.T) {
	script := readFile(t, "configure.sh") + readFile(t, "omp-normalize.cjs") + readFile(t, "image.json")
	for _, required := range []string{
		harness.ConfigureOutputPath,
		harness.ConfigurePreviousConfigPath,
		// Reuse goes through the PREV_ sentinel and comes back as usePrevious,
		// so no credential is read or re-emitted when the existing one is kept.
		`process.env['PREV_' + name]`,
		"usePrevious",
		// The credentials template is JSON, so a quote inside a template action
		// arrives at the renderer backslash-escaped and the parser rejects it.
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
// (ADR 26-10-14-718). Which secrets exist is only known once a user signs in,
// so the manifest declares none, and no baseline credentials file: one would
// land in every sandbox describing credentials with nothing behind them.
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
	if _, ok := files[credentialsPath]; ok {
		t.Fatal("manifest declares a baseline credentials file")
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
	// Browser sign-ins end at fixed localhost ports, which only a forward from
	// the user's machine can reach; each says what to do when it cannot be.
	var ports []int
	for _, port := range image.Harness.Config.Ports {
		ports = append(ports, port.Port)
		if port.Unavailable == "" {
			t.Errorf("port %d says nothing when it cannot be forwarded", port.Port)
		}
	}
	if !slices.Equal(ports, []int{1455, 54545, 8085, 51121, 54549}) {
		t.Fatalf("config ports = %v", ports)
	}
}

// The hook extension is reached through a file in the home that re-exports the
// one the image installs, so the code a sandbox runs is the image's
// (ADR 26-10-14-718 §3).
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

// The policy baseline (ADR 26-10-14-718 §4): an overlay the env names, holding
// scalars only, since a list in an overlay replaces the user's.
func TestImagePolicy(t *testing.T) {
	image := readImage(t)
	if image.Env["PI_CONFIG_FILES"] != overlayPath {
		t.Fatalf("PI_CONFIG_FILES = %q, want %s", image.Env["PI_CONFIG_FILES"], overlayPath)
	}
	// onnxruntime-node's install downloads a CUDA build no sandbox can use,
	// and the version store installs omp inside the sandbox too.
	if image.Env["ONNXRUNTIME_NODE_INSTALL_CUDA"] != "skip" {
		t.Errorf("ONNXRUNTIME_NODE_INSTALL_CUDA = %q, want skip", image.Env["ONNXRUNTIME_NODE_INSTALL_CUDA"])
	}
	// The egress proxy is the pool's to name. A copy here would drift from it,
	// and a build in a sandbox rewrites a loopback proxy address in the build
	// arguments it passes on — the manifest included — to the build network's.
	if v, ok := image.Env["PI_PROXY"]; ok {
		t.Errorf("manifest sets PI_PROXY=%q; it follows HTTPS_PROXY (proxy-env.sh)", v)
	}
	var overlay map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, "discobox.yml")), &overlay); err != nil {
		t.Fatal(err)
	}
	var walk func(prefix string, v any)
	scalars := map[string]any{}
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				walk(prefix+key+".", value)
			}
		case []any:
			t.Errorf("overlay sets the list %s, which would replace the user's", strings.TrimSuffix(prefix, "."))
		default:
			scalars[strings.TrimSuffix(prefix, ".")] = v
		}
	}
	walk("", overlay)
	if scalars["startup.setupWizard"] != false || scalars["startup.checkUpdate"] != false {
		t.Fatalf("overlay = %v, want the setup wizard and the update check off", scalars)
	}
	feature := featuretest.Load(t, ".")
	for source, want := range map[string]string{
		"discobox.yml": overlayPath,
		"proxy-env.sh": "/etc/profile.d/omp-proxy.sh",
		"launch.sh":    "/usr/local/bin/" + harness.RunCommand,
		"prompt.sh":    "/usr/local/bin/discobox-prompt",
		"configure.sh": image.Harness.Config.Command[0],
		"agent.conf":   "/usr/local/libexec/discobox/agent.conf",
	} {
		if got := feature.Installs(source); got != want {
			t.Errorf("the feature installs %s at %q, want %q", source, got, want)
		}
	}
	if !feature.Runs(t, "ONNXRUNTIME_NODE_INSTALL_CUDA=skip npm install -g @oh-my-pi/pi-coding-agent") {
		t.Error("the feature does not install omp with onnxruntime's CUDA download skipped")
	}
	if !strings.Contains(readFile(t, "agent.conf"), "AGENT_BIN=omp\n") {
		t.Error("agent.conf does not name the omp binary")
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). omp sends each positional argument as a message of
// its own and reads an @-led one as a file, so the joined prompt follows `--`.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	policy := []string{"--auto-approve", "--allow-home"}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"--", "fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"--", "fix the failing tests"}},
		{"a dash-led prompt stays a prompt", []string{"-v", "explain this flag"}, []string{"--", "-v explain this flag"}},
		{"an @-led prompt is not a file", []string{"@main.go", "explain"}, []string{"--", "@main.go explain"}},
		{"no prompt passes none", nil, nil},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "omp", nil, tc.args)
			if want := append(slices.Clone(policy), tc.want...); !slices.Equal(got, want) {
				t.Fatalf("omp argv = %#v, want %#v", got, want)
			}
		})
	}
}

// TestLaunchImportsTheCredentials covers the delivery half of ADR 26-10-14-718
// §2: the launcher hands the harness's credentials file to omp's own import
// before it starts omp, on every launch, and a failed import does not keep omp
// from starting.
func TestLaunchImportsTheCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tc := range []struct {
		name        string
		credentials string
		importFails bool
		want        []string
	}{
		{"no credentials file starts omp alone", "", false, []string{"--auto-approve --allow-home"}},
		{"a credentials file is imported first", `{"version":1,"credentials":[]}`, false,
			[]string{"auth import HOME/" + credentialsPath, "--auto-approve --allow-home"}},
		{"a failed import still starts omp", `{"version":1,"credentials":[]}`, true,
			[]string{"auth import HOME/" + credentialsPath, "--auto-approve --allow-home"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home, bin := filepath.Join(dir, "home"), filepath.Join(dir, "bin")
			for _, d := range []string{bin, filepath.Join(home, ".config", "discobox")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.credentials != "" {
				if err := os.WriteFile(filepath.Join(home, credentialsPath), []byte(tc.credentials), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			failure := ""
			if tc.importFails {
				failure = `if [ "$1" = auth ]; then echo "bad export" >&2; exit 1; fi`
			}
			writeExecutable(t, filepath.Join(bin, "omp"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$LOG\"\n"+failure+"\n")
			launcher := filepath.Join(dir, "launch.sh")
			writeExecutable(t, launcher, strings.ReplaceAll(readFile(t, "launch.sh"), "/usr/local/libexec/discobox/omp-capture", "true"))
			cmd := exec.CommandContext(t.Context(), "sh", launcher) //nolint:gosec // The launcher under test.
			log := filepath.Join(dir, "log")
			cmd.Env = append(os.Environ(), "HOME="+home, "LOG="+log, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("launch.sh: %v\n%s", err, out)
			}
			if tc.importFails && !strings.Contains(string(out), "could not import") {
				t.Errorf("a failed import was not reported: %q", out)
			}
			got := strings.Split(strings.TrimSpace(strings.ReplaceAll(readFile(t, log), home, "HOME")), "\n")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("omp ran %q, want %q", got, tc.want)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // A stub the script under test executes.
		t.Fatal(err)
	}
}

// omp's Anthropic transport reads its proxy from PI_PROXY alone and otherwise
// connects direct, which a sandbox's egress does not allow (ADR 26-10-14-718
// §4). PI_PROXY follows HTTPS_PROXY, leaves one the user set alone, and every
// entry point that may not run from a login shell sets it.
func TestPIProxyFollowsHTTPSProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"from HTTPS_PROXY", []string{"HTTPS_PROXY=http://127.0.0.1:17008"}, "http://127.0.0.1:17008"},
		{"a user's own is kept", []string{"HTTPS_PROXY=http://127.0.0.1:17008", "PI_PROXY=http://region.example:3128"}, "http://region.example:3128"},
		{"no proxy, none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "sh", "-c", `. ./proxy-env.sh; printf '%s' "${PI_PROXY-}"`)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tc.env...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("PI_PROXY = %q, want %q", out, tc.want)
			}
		})
	}
	for _, script := range []string{"launch.sh", "configure.sh", "prompt.sh"} {
		if !strings.Contains(readFile(t, script), ". /etc/profile.d/omp-proxy.sh") {
			t.Errorf("%s does not set PI_PROXY", script)
		}
	}
}
