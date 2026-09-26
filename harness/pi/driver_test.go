package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
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
	for _, required := range []string{
		harness.ConfigureOutputPath,
		harness.ConfigurePreviousConfigPath,
		// Reuse goes through the PREV_ sentinel and comes back as usePrevious,
		// so no credential is read or re-emitted when the existing one is kept.
		`process.env['PREV_' + envName]`,
		"usePrevious",
		// The credentials template is JSON, so a quote inside a template action
		// arrives at the renderer backslash-escaped and the parser rejects it.
		"{{ .secrets.${envName} }}",
		// Every retry is a person asking for one.
		"confirm_retry",
		// The check is a one-shot print, with nothing piped into it.
		"pi --print --no-session",
		"</dev/null",
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
	segments := strings.Split(loop, "continue")
	for _, leadingUpToAContinue := range segments[:len(segments)-1] {
		if !strings.Contains(leadingUpToAContinue, "confirm_retry") {
			t.Fatal("configure script's loop continues without confirm_retry, so it can spin")
		}
	}
}

// TestImageDeclaresNoCredentials pins the half of the contract the image owns.
// Which secrets exist is only known once a user signs in to providers, so the
// manifest declares none, and no baseline auth.json: one would land in every
// sandbox describing credentials with nothing behind them.
func TestImageDeclaresNoCredentials(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
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
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if want := (Driver{}).Definition().ID; image.Harness.ID != want {
		t.Fatalf("manifest id = %q, want the definition's %q", image.Harness.ID, want)
	}
	if len(image.Harness.Secrets) != 0 {
		t.Fatalf("manifest declares secrets %v, want none", image.Harness.Secrets)
	}
	// The one baseline file is Discobox's settings, edited in the declared set
	// until a configure run returns its own copy — so it is rewritten on every
	// launch rather than kept from the first, or an edit would never land.
	if len(image.Harness.Files) != 1 || image.Harness.Files[0].Path != settingsPath ||
		image.Harness.Files[0].CreateOnly || image.Harness.Files[0].Template {
		t.Fatalf("manifest files = %+v, want only the settings file, plain", image.Harness.Files)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(image.Harness.Files[0].Content), &settings); err != nil {
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
	// The version a sandbox runs comes from the pool-cached store (ADR 0114),
	// so pi's own version check stays off.
	if image.Env["PI_SKIP_VERSION_CHECK"] != "1" {
		t.Fatalf("manifest env = %v, want pi's version check disabled", image.Env)
	}
	// Every sandbox is a fresh install to pi, and each would otherwise report
	// itself to pi.dev on first start.
	if image.Env["PI_TELEMETRY"] != "0" {
		t.Fatalf("manifest env = %v, want pi's install ping off", image.Env)
	}
	// ChatGPT's and Anthropic's browser sign-ins end at fixed localhost ports,
	// which only a forward from the user's machine can reach.
	var ports []int
	for _, port := range image.Harness.Config.Ports {
		ports = append(ports, port.Port)
		if port.Unavailable == "" {
			t.Errorf("port %d says nothing when it cannot be forwarded", port.Port)
		}
	}
	if !slices.Equal(ports, []int{1455, 53692}) {
		t.Fatalf("config ports = %v, want [1455 53692]", ports)
	}
	if !slices.Equal(image.Harness.Config.Command, []string{"/usr/local/libexec/discobox/configure-pi"}) {
		t.Fatalf("config command = %v", image.Harness.Config.Command)
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). pi joins positional messages itself but reads one
// beginning with @ as a file, so the joined prompt is one argument after `--`.
// The image's hook extension is named on every launch, and the project's own
// configuration is trusted.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	baseline := []string{"--approve", "--extension", "/usr/local/libexec/discobox/pi-hook-extension.js"}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, append(slices.Clone(baseline), "--", "fix the failing tests")},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, append(slices.Clone(baseline), "--", "fix the failing tests")},
		{"a prompt that begins with @ stays a prompt", []string{"@channel", "say", "hi"}, append(slices.Clone(baseline), "--", "@channel say hi")},
		{"no prompt passes none", nil, baseline},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, append(slices.Clone(baseline), "--continue")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "pi", nil, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("pi argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// signedInAuth is auth.json as /login leaves it for five providers, one of
// each shape the configure flow handles differently.
const signedInAuth = `{
  "anthropic": {"type": "oauth", "refresh": "real-anthropic-refresh", "access": "real-anthropic-access", "expires": 1900000000000},
  "openai-codex": {"type": "oauth", "refresh": "real-openai-refresh", "access": "real-openai-access", "expires": 1900000000000, "accountId": "acct-1"},
  "github-copilot": {"type": "oauth", "refresh": "real-gh-token", "access": "real-copilot-token", "expires": 1700000000000, "enterpriseUrl": null},
  "kimi-coding": {"type": "oauth", "refresh": "real-kimi-refresh", "access": "real-kimi-access", "expires": 1900000000000},
  "zai": {"type": "api_key", "key": "real-zai-key"},
  "vault": {"type": "api_key", "key": "!security find-generic-password -ws vault"}
}`

// TestConfigureCapturesEverySignedInProvider runs configure.sh against a
// stubbed pi that "signs in" providers the way /login leaves them, and checks
// what the harness config would store and deliver.
func TestConfigureCapturesEverySignedInProvider(t *testing.T) {
	env := newConfigureEnv(t)
	env.login = signedInAuth
	out := env.run(t, nil, nil)

	secrets := map[string]map[string]any{}
	for _, secret := range out.Secrets {
		secrets[secret["envName"].(string)] = secret
	}
	value := func(name string) map[string]any {
		t.Helper()
		secret, ok := secrets[name]
		if !ok {
			t.Fatalf("no secret %s in %v", name, out.Secrets)
		}
		v, _ := secret["value"].(map[string]any)
		return v
	}
	if v := value("PI_ANTHROPIC_CREDENTIAL"); secrets["PI_ANTHROPIC_CREDENTIAL"]["type"] != "oauth" ||
		v["token"] != "real-anthropic-access" || v["refreshToken"] != "real-anthropic-refresh" ||
		v["tokenUrl"] != "https://platform.claude.com/v1/oauth/token" || v["clientId"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Fatalf("anthropic secret = %v", secrets["PI_ANTHROPIC_CREDENTIAL"])
	}
	if v := value("PI_OPENAI_CODEX_CREDENTIAL"); secrets["PI_OPENAI_CODEX_CREDENTIAL"]["type"] != "oauth" ||
		v["tokenUrl"] != "https://auth.openai.com/oauth/token" || v["tokenRequestEncoding"] != nil {
		t.Fatalf("openai-codex secret = %v", secrets["PI_OPENAI_CODEX_CREDENTIAL"])
	}
	// Copilot's credential is its GitHub token, which pi mints access tokens
	// from; the minted one lives half an hour and is not worth keeping.
	if v := value("PI_GITHUB_COPILOT_CREDENTIAL"); secrets["PI_GITHUB_COPILOT_CREDENTIAL"]["type"] != "token" || v["token"] != "real-gh-token" {
		t.Fatalf("copilot secret = %v, want a token holding the GitHub token", secrets["PI_GITHUB_COPILOT_CREDENTIAL"])
	}
	// A sign-in the control plane cannot renew is a plain token.
	if v := value("PI_KIMI_CODING_CREDENTIAL"); secrets["PI_KIMI_CODING_CREDENTIAL"]["type"] != "token" || v["token"] != "real-kimi-access" {
		t.Fatalf("kimi secret = %v, want a token", secrets["PI_KIMI_CODING_CREDENTIAL"])
	}
	if v := value("PI_ZAI_CREDENTIAL"); secrets["PI_ZAI_CREDENTIAL"]["type"] != "token" || v["token"] != "real-zai-key" {
		t.Fatalf("zai secret = %v, want a token", secrets["PI_ZAI_CREDENTIAL"])
	}
	// A key that is a command cannot be captured, and is not.
	if _, ok := secrets["PI_VAULT_CREDENTIAL"]; ok {
		t.Fatalf("secrets = %v, want no secret for a key that is a command", out.Secrets)
	}

	// Rendered the way a sandbox renders it, auth.json carries sentinels and
	// nothing real.
	rendered := out.renderAuth(t, map[string]string{ //nolint:gosec // Sentinels a test renders into the file, not credentials.
		"PI_ANTHROPIC_CREDENTIAL":      "S-ANTHROPIC",
		"PI_OPENAI_CODEX_CREDENTIAL":   "S-CODEX",
		"PI_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PI_KIMI_CODING_CREDENTIAL":    "S-KIMI",
		"PI_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if strings.Contains(rendered, "real-") || strings.Contains(rendered, "security find-generic-password") {
		t.Fatalf("delivered auth.json holds a real credential: %s", rendered)
	}
	var auth map[string]map[string]any
	if err := json.Unmarshal([]byte(rendered), &auth); err != nil {
		t.Fatalf("rendered auth.json is not JSON: %v\n%s", err, rendered)
	}
	if a := auth["anthropic"]; a["access"] != "S-ANTHROPIC" || a["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		a["expires"] != float64(4102444800000) || a["type"] != "oauth" {
		t.Fatalf("delivered anthropic entry = %v", a)
	}
	if c := auth["openai-codex"]; c["access"] != "S-CODEX" || c["accountId"] != "acct-1" {
		t.Fatalf("delivered openai-codex entry = %v, want the account carried with the sentinel", c)
	}
	if gh := auth["github-copilot"]; gh["access"] != "S-GH" || gh["refresh"] != "S-GH" || gh["expires"] != float64(0) {
		t.Fatalf("delivered copilot entry = %v, want the sentinel in both token fields and no expiry", gh)
	}
	if k := auth["kimi-coding"]; k["access"] != "S-KIMI" || k["expires"] != float64(4102444800000) {
		t.Fatalf("delivered kimi entry = %v, want the sentinel never to look stale", k)
	}
	if z := auth["zai"]; z["key"] != "S-ZAI" || z["type"] != "api_key" {
		t.Fatalf("delivered zai entry = %v", z)
	}
	if _, ok := auth["vault"]; ok {
		t.Fatalf("delivered auth.json = %v, want no entry for the command key", auth)
	}
	if _, ok := out.file(".pi/agent/settings.json"); !ok {
		t.Fatal("configure did not return the settings the user left")
	}
	// The check is of pi's default model — what a sandbox starts on — so it
	// names none, and pipes nothing in.
	printArgs, err := os.ReadFile(filepath.Join(env.dir, "print-args"))
	if err != nil {
		t.Fatalf("configure never checked the default model: %v", err)
	}
	if strings.Contains(string(printArgs), "--model") {
		t.Fatalf("configure checked a chosen model rather than the default: %s", printArgs)
	}

	// Reconfigure: the same harness, a user who changes nothing. Every
	// credential is seeded as its sentinel and comes back as usePrevious.
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	env.login = ""
	again := env.run(t, previous, map[string]string{ //nolint:gosec // Sentinels a test seeds, not credentials.
		"PREV_PI_ANTHROPIC_CREDENTIAL":      "S-ANTHROPIC",
		"PREV_PI_OPENAI_CODEX_CREDENTIAL":   "S-CODEX",
		"PREV_PI_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PREV_PI_KIMI_CODING_CREDENTIAL":    "S-KIMI",
		"PREV_PI_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if len(again.Secrets) != 5 {
		t.Fatalf("reconfigure secrets = %v, want the five kept", again.Secrets)
	}
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true || secret["value"] != nil {
			t.Fatalf("reconfigure secret %v, want usePrevious and no value", secret)
		}
	}
	before, _ := out.file(".pi/agent/auth.json")
	after, _ := again.file(".pi/agent/auth.json")
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}
}

// TestConfigureKeepsProvidersWhenTheDefaultModelFails covers a default model
// that does not answer — a plan or region restriction, not a bad credential.
// It is reported, and declining to start pi again saves every provider.
func TestConfigureKeepsProvidersWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	env.login = `{"opencode": {"type": "api_key", "key": "real-go-key"}, "zai": {"type": "api_key", "key": "real-zai-key"}}`
	out := env.runWithInput(t, nil, map[string]string{"PI_STUB_PRINT_FAILS": "1"}, "\nn\n")
	var names []string
	for _, secret := range out.Secrets {
		names = append(names, secret["envName"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"PI_OPENCODE_CREDENTIAL", "PI_ZAI_CREDENTIAL"}) {
		t.Fatalf("secrets = %v, want both providers kept", names)
	}
}

// TestConfigureKeepsTheEditedJudgeModel covers Discobox's settings file
// surviving a reconfigure: a judge model someone set by editing it is theirs.
func TestConfigureKeepsTheEditedJudgeModel(t *testing.T) {
	env := newConfigureEnv(t)
	env.login = `{"zai": {"type": "api_key", "key": "real-zai-key"}}`
	env.homeFiles = map[string]string{
		settingsPath: `{"judgeModel": "anthropic/claude-opus-5"}`,
	}
	out := env.run(t, nil, nil)
	file, ok := out.file(settingsPath)
	if !ok {
		t.Fatal("configure returned no settings file")
	}
	var settings struct {
		JudgeModel string `json:"judgeModel"`
	}
	if err := json.Unmarshal([]byte(file["content"].(string)), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.JudgeModel != "anthropic/claude-opus-5" {
		t.Fatalf("settings = %+v, want the edited judge kept", settings)
	}
}

// settingsPath is Discobox's settings file for the harness, home-relative.
const settingsPath = ".config/discobox/pi-harness.json"

// TestPromptJudgesWithTheConfiguredModel covers discobox-prompt's judge role:
// the model the settings file names, else pi's own default from its settings
// — read before the judge is isolated from the state it lives in — else pi's
// own pick, when neither names one.
func TestPromptJudgesWithTheConfiguredModel(t *testing.T) {
	args := `for arg in "$@"; do printf '[%s]' "$arg"; done; printf '\n'`
	isolated := "[--no-tools][--no-extensions][--no-skills][--no-prompt-templates][--no-themes][--no-context-files][--no-approve]"
	for _, tc := range []struct {
		name, setting, piSettings, want string
	}{
		{"nothing named leaves the model to pi", "", "",
			"[--print][--no-session][--thinking][off][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"judgeModel wins", `{"judgeModel": "anthropic/claude-opus-5"}`, `{"defaultProvider": "openai", "defaultModel": "gpt-5.6"}`,
			"[--print][--no-session][--thinking][off][--model][anthropic/claude-opus-5][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"pi's default provider and model", `{"judgeModel": ""}`, `{"defaultProvider": "openai", "defaultModel": "gpt-5.6"}`,
			"[--print][--no-session][--thinking][off][--model][openai/gpt-5.6][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"pi's default model alone", "", `{"defaultModel": "gpt-5.6"}`,
			"[--print][--no-session][--thinking][off][--model][gpt-5.6][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		// pi reads an option's value as the next word and has no --model=NAME
		// spelling, so a value that would be read as a flag is dropped rather
		// than passed.
		{"a model that reads like a flag is not handed to pi", "", `{"defaultModel": "--extension=x"}`,
			"[--print][--no-session][--thinking][off][--system-prompt][decide]" + isolated + "[--][is it safe]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := t.TempDir()
			if tc.piSettings != "" {
				if err := os.WriteFile(filepath.Join(agent, "settings.json"), []byte(tc.piSettings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := runWrapper(t, "prompt.sh", tc.setting, args, map[string]string{"PI_CODING_AGENT_DIR": agent},
				"--model", "judge", "--no-tools", "--system", "decide", "--prompt", "is it safe")
			if got != tc.want {
				t.Errorf("pi argv %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPromptIsolatesTheToolFreeJudge covers what --no-tools shuts out: the
// caller is the agent being judged, so no configuration, instruction,
// extension or PI_* variable it could have set may reach the judge — only the
// credentials from its auth.json, without a key that is a command pi would
// run.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	stub := `printf 'cwd-empty=%s agent=%s offline=%s leaked=%s stdin=%s\n' \
		"$(ls -A | wc -l | tr -d ' ')" "$(ls -A "$PI_CODING_AGENT_DIR" | tr '\n' ' ')$(jq -c 'to_entries | map(.key + ":" + .value.type)' "$PI_CODING_AGENT_DIR/auth.json")" \
		"${PI_OFFLINE-}" "${PI_SKIP_VERSION_CHECK-}${PI_TELEMETRY-}${PI_PACKAGE_DIR-}" "$(cat)"`
	agent := t.TempDir()
	if err := os.WriteFile(filepath.Join(agent, "auth.json"), []byte(`{
		"anthropic": {"type": "oauth", "access": "S", "refresh": "r", "expires": 1},
		"zai": {"type": "api_key", "key": "S2"},
		"vault": {"type": "api_key", "key": "!cat /etc/passwd"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent, "settings.json"), []byte(`{"extensions": ["/tmp/evil.ts"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runWrapperWithStdin(t, "prompt.sh", "", stub, map[string]string{
		"PI_CODING_AGENT_DIR":   agent,
		"PI_SKIP_VERSION_CHECK": "1",
		"PI_TELEMETRY":          "0",
		"PI_PACKAGE_DIR":        "/agent-packages",
	}, "planted on stdin", "--model", "judge", "--no-tools", "--prompt", "is it safe")
	want := `cwd-empty=0 agent=auth.json ["anthropic:oauth","zai:api_key"] offline=1 leaked= stdin=`
	if got != want {
		t.Fatalf("judge ran with\n  %s\nwant\n  %s", got, want)
	}
}

// runWrapper runs one of the image's wrappers with the settings file holding
// setting (none when empty), extra in its environment, and a stubbed pi whose
// body is stub, and returns what the stub printed.
func runWrapper(t *testing.T, script, setting, stub string, extra map[string]string, args ...string) string {
	t.Helper()
	return runWrapperWithStdin(t, script, setting, stub, extra, "", args...)
}

func runWrapperWithStdin(t *testing.T, script, setting, stub string, extra map[string]string, stdin string, args ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run the wrapper with", tool)
		}
	}
	dir := t.TempDir()
	home, bin := filepath.Join(dir, "home"), filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if setting != "" {
		path := filepath.Join(home, settingsPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(setting), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "pi"), "#!/bin/sh\n"+stub+"\n")
	scriptPath, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{scriptPath}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	env := []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PI_") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return strings.TrimSpace(string(out))
}

type configureEnv struct {
	dir    string
	home   string
	bin    string
	runDir string
	script string
	// login is the auth.json the stubbed interactive pi writes, standing in
	// for what the user signs in to in the session.
	login string
	// homeFiles are written into the sandbox home before the script runs:
	// what a configured harness delivers into its configure sandbox.
	homeFiles map[string]string
}

func newConfigureEnv(t *testing.T) *configureEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "node", "timeout", "grep", "tail", "mktemp", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run the configure script with", tool)
		}
	}
	dir := t.TempDir()
	env := &configureEnv{
		dir:    dir,
		home:   filepath.Join(dir, "home"),
		bin:    filepath.Join(dir, "bin"),
		runDir: filepath.Join(dir, "run") + "/",
		script: filepath.Join(dir, "configure.sh"),
	}
	for _, d := range []string{env.bin, env.runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, env.script, strings.ReplaceAll(string(raw), "/run/discobox/configure/", env.runDir))
	// `script -q -e -c CMD /dev/null` runs CMD with a terminal; here it just
	// runs it.
	writeExecutable(t, filepath.Join(env.bin, "script"), "#!/bin/sh\nexec sh -c \"$4\"\n")
	// The stubbed pi: a bare launch "signs in" providers by writing
	// $PI_STUB_LOGIN as auth.json, and `--print` answers the check.
	writeExecutable(t, filepath.Join(env.bin, "pi"), `#!/bin/sh
case "$*" in
--print*)
	printf '%s\n' "$*" >>"$HOME/../print-args"
	if [ -n "${PI_STUB_PRINT_FAILS:-}" ]; then echo "Error: this model needs an opt-in" >&2; exit 1; fi
	echo "discobox-ok" ;;
"")
	mkdir -p "$HOME/.pi/agent"
	echo '{"defaultProvider":"openai","defaultModel":"gpt-5.6-terra"}' >"$HOME/.pi/agent/settings.json"
	if [ -n "${PI_STUB_LOGIN:-}" ]; then printf '%s\n' "$PI_STUB_LOGIN" >"$HOME/.pi/agent/auth.json"; fi ;;
*) echo "unexpected pi $*" >&2; exit 1 ;;
esac
`)
	return env
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // A stub the script under test executes.
		t.Fatal(err)
	}
}

type configureOutput struct {
	Files   []map[string]any `json:"files"`
	Secrets []map[string]any `json:"secrets"`
}

// run runs configure.sh in a fresh sandbox home, with previous as the seeded
// previous configuration and extra in the environment, and returns its output.
func (e *configureEnv) run(t *testing.T, previous map[string]any, extra map[string]string) configureOutput {
	t.Helper()
	return e.runWithInput(t, previous, extra, "\n")
}

// runWithInput is run with stdin: one line per question the script asks.
func (e *configureEnv) runWithInput(t *testing.T, previous map[string]any, extra map[string]string, input string) configureOutput {
	t.Helper()
	if err := os.RemoveAll(e.home); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.home, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range e.homeFiles {
		path := filepath.Join(e.home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"harness-configure.json", "harness-previous-config.json"} {
		_ = os.Remove(e.runDir + name)
	}
	if previous != nil {
		raw, err := json.Marshal(previous)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.runDir+"harness-previous-config.json", raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "sh", e.script) //nolint:gosec // The configure script under test, copied into this test's directory.
	cmd.Env = append(os.Environ(),
		"HOME="+e.home,
		"PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PI_STUB_LOGIN="+e.login,
		"NO_COLOR=1",
	)
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader(input)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("configure.sh: %v\n%s", err, combined)
	}
	raw, err := os.ReadFile(e.runDir + "harness-configure.json")
	if err != nil {
		t.Fatalf("configure wrote no output: %v\n%s", err, combined)
	}
	var out configureOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (o configureOutput) file(path string) (map[string]any, bool) {
	for _, file := range o.Files {
		if file["path"] == path {
			return file, true
		}
	}
	return nil, false
}

// renderAuth renders the returned auth.json the way the sandbox agent renders
// a templated harness file (sandbox-agent/terminal).
func (o configureOutput) renderAuth(t *testing.T, sentinels map[string]string) string {
	t.Helper()
	file, ok := o.file(".pi/agent/auth.json")
	if !ok || file["template"] != true {
		t.Fatalf("no templated auth.json in %v", o.Files)
	}
	tmpl, err := template.New("auth").Funcs(template.FuncMap{"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		return string(raw), err
	}}).Option("missingkey=zero").Parse(file["content"].(string))
	if err != nil {
		t.Fatalf("auth.json template does not parse: %v", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, map[string]any{"secrets": sentinels}); err != nil {
		t.Fatal(err)
	}
	return rendered.String()
}

// stripValues is what the control plane seeds as the previous configuration:
// the secrets' metadata, never their values.
func stripValues(secrets []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(secrets))
	for _, secret := range secrets {
		kept := map[string]any{}
		for key, value := range secret {
			if key != "value" {
				kept[key] = value
			}
		}
		out = append(out, kept)
	}
	return out
}

// extensionSet reads one `const NAME = new Set([...])` out of the extension's
// source.
func extensionSet(t *testing.T, text, name string) []string {
	t.Helper()
	first := strings.Index(text, name+" = new Set([")
	if first < 0 {
		t.Fatalf("extension declares no %s", name)
	}
	last := strings.Index(text[first:], "])")
	if last < 0 {
		t.Fatalf("%s is not terminated", name)
	}
	var out []string
	for _, line := range strings.Split(text[first:first+last], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		if event := strings.Trim(strings.TrimSuffix(line, ","), `"`); event != "" {
			out = append(out, event)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty", name)
	}
	return out
}

// publishedExtensionEvents reads the event names the extension publishes
// straight out of its source: the allowlist it declares.
func publishedExtensionEvents(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("hook-extension.js")
	if err != nil {
		t.Fatal(err)
	}
	return extensionSet(t, string(source), "PUBLISHED_EVENTS")
}

// The extension, the mapping table, and ADR 0146's rule are one thing in three
// places. Every event the extension publishes must either carry a canonical
// name or be listed here as having none — so adding one without deciding what
// it is called across harnesses fails rather than quietly recording a hook
// nothing portable can match.
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	// pi facts Claude Code has no word for. Each is still recorded and still
	// waitable under pi's own name (ADR 0146 §3).
	noCanonicalName := []string{
		"session_info_changed",
		"agent_start",
		"agent_end",
		"turn_start",
		"turn_end",
		"session_compact_failed",
		"model_select",
	}
	published := publishedExtensionEvents(t)
	for _, event := range published {
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
		if !slices.Contains(published, event) {
			t.Errorf("noCanonicalName lists %s, which the extension does not publish", event)
		}
	}
}

// The mapping table must not name a pi event the extension never publishes: a
// translation for an event nothing emits is a claim about pi that nothing
// tests.
func TestMappedEventsAreAllPublished(t *testing.T) {
	published := publishedExtensionEvents(t)
	for _, event := range []string{
		"session_start", "session_shutdown", "before_agent_start", "agent_settled",
		"tool_call", "tool_result", "session_before_compact", "session_compact",
	} {
		if harness.CanonicalHookEvent(Driver{}.ID(), event) == "" {
			t.Errorf("event %s lost its canonical name", event)
		}
		if !slices.Contains(published, event) {
			t.Errorf("event %s has a canonical name but the extension never publishes it", event)
		}
	}
}

// The stream-rate events are the reason the extension keeps an allowlist at
// all: message_update fires for every delta of every message, and publishing
// one spawns a process and writes a row.
func TestStreamRateEventsAreNotPublished(t *testing.T) {
	published := publishedExtensionEvents(t)
	for _, event := range []string{
		"message_start", "message_update", "message_end", "tool_execution_update",
		"provider_stream_event", "context", "context_with_system", "before_provider_request",
		"before_provider_headers", "after_provider_response", "input", "user_bash",
	} {
		if slices.Contains(published, event) {
			t.Errorf("extension publishes %s, which is excluded on purpose", event)
		}
	}
}

// Every hook this image publishes names pi as its provider, which is what the
// mapping is keyed by and what `discobox admin audit hooks --provider` selects
// on; and the launcher is what loads the extension, by the path the Dockerfile
// installs it at.
func TestExtensionPublishesUnderTheDriverProvider(t *testing.T) {
	source, err := os.ReadFile("hook-extension.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `const PROVIDER = "`+Driver{}.ID()+`"`) {
		t.Errorf("extension does not publish under the %s provider", Driver{}.ID())
	}
	if !strings.Contains(string(source), `spawn("discobox-hook-publish", ["--provider", PROVIDER, "--event", event]`) {
		t.Error("extension does not publish through the generic publisher")
	}
	const installed = "/usr/local/libexec/discobox/pi-hook-extension.js"
	launcher, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launcher), "HOOK_EXTENSION="+installed) {
		t.Errorf("launcher does not load the extension from %s", installed)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "hook-extension.js "+installed) {
		t.Errorf("Dockerfile does not install the extension at %s", installed)
	}
}

// What the extension does, not what it says: it runs against a synthetic pi,
// and the events it published, with their payloads, are what is checked. A
// stream-rate event is published from nowhere, and a payload carries the
// subject of its event and not the conversation.
func TestExtensionPublishesTheEventsItDeclares(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the extension's behavior is not exercised")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "extension-driver.mjs")).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("run extension driver: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("run extension driver: %v", err)
	}
	var published []struct {
		Provider string         `json:"provider"`
		Event    string         `json:"event"`
		Payload  map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &published); err != nil {
		t.Fatalf("parse driver output %q: %v", out, err)
	}
	var events []string
	for _, p := range published {
		if p.Provider != (Driver{}).ID() {
			t.Errorf("event %s published under provider %q", p.Event, p.Provider)
		}
		events = append(events, p.Event)
	}
	want := []string{
		"session_start", "before_agent_start", "agent_start", "turn_start", "tool_call", "tool_result",
		"turn_end", "agent_end", "agent_settled", "model_select", "session_shutdown",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("published\n got %v\nwant %v", events, want)
	}
	byEvent := map[string]map[string]any{}
	for _, p := range published {
		byEvent[p.Event] = p.Payload
	}
	if prompt := byEvent["before_agent_start"]; prompt["prompt"] != "fix the tests" || prompt["images"] != nil || prompt["systemPrompt"] != "you are pi" {
		t.Errorf("before_agent_start payload = %v, want the prompt and not the images", prompt)
	}
	if call := byEvent["tool_call"]; call["toolName"] != "bash" || call["toolCallId"] != "call_1" {
		t.Errorf("tool_call payload = %v", call)
	} else if input, _ := call["input"].(map[string]any); input["command"] != "ls -la" {
		t.Errorf("tool_call payload = %v, want the tool's input", call)
	}
	if result := byEvent["tool_result"]; result["content"] != nil || result["isError"] != false {
		t.Errorf("tool_result payload = %v, want the outcome and not the content", result)
	} else if input, _ := result["input"].(map[string]any); input["command"] != "ls -la" {
		t.Errorf("tool_result payload = %v, want the tool's input", result)
	}
	if end := byEvent["agent_end"]; end["messages"] != nil || end["willRetry"] != false {
		t.Errorf("agent_end payload = %v, want no transcript", end)
	}
	if selected, _ := byEvent["model_select"]["model"].(map[string]any); selected["provider"] != "anthropic" || selected["id"] != "claude-sonnet-5" {
		t.Errorf("model_select payload = %v", byEvent["model_select"])
	}
}
