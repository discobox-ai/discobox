package opencode

import (
	"bytes"
	"encoding/json"
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

// TestImageDeclaresNoCredentials pins the half of the contract the image owns.
// Which secrets exist is only known once a user connects providers, so the
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
	if settings["judgeModel"] != "" || settings["webSearch"] != nil {
		t.Fatalf("baseline settings = %v, want the default judge and opencode's own web search default", settings)
	}
	// The manifest names no command: the launcher is installed under the
	// conventional name and the runtime types that (ADR 0086 §3).
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	// The version a sandbox runs comes from the pool-cached store (ADR 0114),
	// so opencode's own updater stays off.
	if image.Env["OPENCODE_DISABLE_AUTOUPDATE"] != "1" {
		t.Fatalf("manifest env = %v, want opencode's updater disabled", image.Env)
	}
	// ChatGPT's and DigitalOcean's browser sign-ins end at fixed localhost
	// ports, which only a forward from the user's machine can reach.
	var ports []int
	for _, port := range image.Harness.Config.Ports {
		ports = append(ports, port.Port)
		if port.Unavailable == "" {
			t.Errorf("port %d says nothing when it cannot be forwarded", port.Port)
		}
	}
	if !slices.Equal(ports, []int{1455, 1456}) {
		t.Fatalf("config ports = %v, want [1455 1456]", ports)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"npm install -g opencode-ai",
		"launch.sh /usr/local/bin/" + harness.RunCommand,
		"prompt.sh /usr/local/bin/discobox-prompt",
		"configure.sh " + image.Harness.Config.Command[0],
	} {
		if !strings.Contains(string(dockerfile), want) {
			t.Errorf("Dockerfile does not install %q", want)
		}
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). The stubbed agent has no server to start a session
// on, so a prompt takes the launcher's fallback — --prompt, which opencode
// leaves in the input box — and arrives there as one prompt.

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). opencode's positional argument is the project
// directory, so the joined prompt goes to --prompt.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"--auto", "--prompt", "fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"--auto", "--prompt", "fix the failing tests"}},
		{"no prompt passes none", nil, []string{"--auto"}},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--auto", "--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "opencode", tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("opencode argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// connectedAuth is auth.json as /connect leaves it for four providers, one of
// each shape the configure flow handles differently.
const connectedAuth = `{
  "openai": {"type": "oauth", "refresh": "real-openai-refresh", "access": "real-openai-access", "expires": 1900000000000, "accountId": "acct-1"},
  "xai": {"type": "oauth", "refresh": "real-xai-refresh", "access": "real-xai-access", "expires": 1900000000000},
  "github-copilot": {"type": "oauth", "refresh": "real-gh-token", "access": "real-gh-token", "expires": 0},
  "zai/": {"type": "api", "key": "real-zai-key"}
}`

// TestConfigureCapturesEveryConnectedProvider runs configure.sh against a
// stubbed opencode that "connects" providers the way /connect leaves them, and
// checks what the harness config would store and deliver.
func TestConfigureCapturesEveryConnectedProvider(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = connectedAuth
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
	if v := value("OPENCODE_OPENAI_CREDENTIAL"); secrets["OPENCODE_OPENAI_CREDENTIAL"]["type"] != "oauth" ||
		v["token"] != "real-openai-access" || v["refreshToken"] != "real-openai-refresh" ||
		v["tokenUrl"] != "https://auth.openai.com/oauth/token" || v["tokenRequestEncoding"] != nil {
		t.Fatalf("openai secret = %v", secrets["OPENCODE_OPENAI_CREDENTIAL"])
	}
	if v := value("OPENCODE_XAI_CREDENTIAL"); secrets["OPENCODE_XAI_CREDENTIAL"]["type"] != "oauth" || v["tokenRequestEncoding"] != "form" {
		t.Fatalf("xai secret = %v, want an oauth secret refreshed form-encoded", secrets["OPENCODE_XAI_CREDENTIAL"])
	}
	// Copilot's token does not expire and has nothing to refresh with.
	if v := value("OPENCODE_GITHUB_COPILOT_CREDENTIAL"); secrets["OPENCODE_GITHUB_COPILOT_CREDENTIAL"]["type"] != "token" || v["token"] != "real-gh-token" {
		t.Fatalf("copilot secret = %v, want a token", secrets["OPENCODE_GITHUB_COPILOT_CREDENTIAL"])
	}
	// opencode reads a provider key with or without its trailing slash.
	if v := value("OPENCODE_ZAI_CREDENTIAL"); secrets["OPENCODE_ZAI_CREDENTIAL"]["type"] != "token" || v["token"] != "real-zai-key" {
		t.Fatalf("zai secret = %v, want a token", secrets["OPENCODE_ZAI_CREDENTIAL"])
	}

	// Rendered the way a sandbox renders it, auth.json carries sentinels and
	// nothing real.
	rendered := out.renderAuth(t, map[string]string{
		"OPENCODE_OPENAI_CREDENTIAL":         "S-OPENAI",
		"OPENCODE_XAI_CREDENTIAL":            "S-XAI",
		"OPENCODE_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"OPENCODE_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if strings.Contains(rendered, "real-") {
		t.Fatalf("delivered auth.json holds a real credential: %s", rendered)
	}
	var auth map[string]map[string]any
	if err := json.Unmarshal([]byte(rendered), &auth); err != nil {
		t.Fatalf("rendered auth.json is not JSON: %v\n%s", err, rendered)
	}
	if openai := auth["openai"]; openai["access"] != "S-OPENAI" || openai["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		openai["expires"] != float64(4102444800000) || openai["accountId"] != "acct-1" {
		t.Fatalf("delivered openai entry = %v", openai)
	}
	if gh := auth["github-copilot"]; gh["access"] != "S-GH" || gh["refresh"] != "S-GH" || gh["expires"] != float64(0) {
		t.Fatalf("delivered copilot entry = %v, want the sentinel in both token fields and no expiry", gh)
	}
	if zai := auth["zai"]; zai["key"] != "S-ZAI" || zai["type"] != "api" {
		t.Fatalf("delivered zai entry = %v", zai)
	}
	if _, ok := out.file(".config/opencode/opencode.json"); !ok {
		t.Fatal("configure did not return the settings the user left")
	}
	// The check is of opencode's default model — what a sandbox starts on —
	// so it names none.
	runArgs, err := os.ReadFile(filepath.Join(env.dir, "run-args"))
	if err != nil {
		t.Fatalf("configure never checked the default model: %v", err)
	}
	if strings.Contains(string(runArgs), "--model") {
		t.Fatalf("configure checked a chosen model rather than the default: %s", runArgs)
	}

	// Reconfigure: the same harness, a user who changes nothing. Every
	// credential is seeded as its sentinel and comes back as usePrevious.
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	env.connect = ""
	again := env.run(t, previous, map[string]string{
		"PREV_OPENCODE_OPENAI_CREDENTIAL":         "S-OPENAI",
		"PREV_OPENCODE_XAI_CREDENTIAL":            "S-XAI",
		"PREV_OPENCODE_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PREV_OPENCODE_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if len(again.Secrets) != 4 {
		t.Fatalf("reconfigure secrets = %v, want the four kept", again.Secrets)
	}
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true || secret["value"] != nil {
			t.Fatalf("reconfigure secret %v, want usePrevious and no value", secret)
		}
	}
	before, _ := out.file(".local/share/opencode/auth.json")
	after, _ := again.file(".local/share/opencode/auth.json")
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}
}

// TestConfigureKeepsProvidersWhenTheDefaultModelFails covers a default model
// that does not answer — a plan or region restriction, not a bad credential.
// It is reported, and declining to start opencode again saves every provider.
func TestConfigureKeepsProvidersWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = `{"opencode-go": {"type": "api", "key": "real-go-key"}, "zai": {"type": "api", "key": "real-zai-key"}}`
	out := env.runWithInput(t, nil, map[string]string{"OPENCODE_STUB_RUN_FAILS": "1"}, "\nn\n\n")
	var names []string
	for _, secret := range out.Secrets {
		names = append(names, secret["envName"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"OPENCODE_OPENCODE_GO_CREDENTIAL", "OPENCODE_ZAI_CREDENTIAL"}) {
		t.Fatalf("secrets = %v, want both providers kept", names)
	}
}

// TestConfigureAsksAboutWebSearch covers the web search question: the answer
// is recorded in Discobox's settings for the harness, and a judge model someone
// set by editing that file survives the reconfigure.
func TestConfigureAsksAboutWebSearch(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = `{"zai": {"type": "api", "key": "real-zai-key"}}`
	env.homeFiles = map[string]string{
		settingsPath: `{"judgeModel": "anthropic/claude-opus-5", "webSearch": true}`,
	}
	for _, tc := range []struct {
		answer string
		want   bool
	}{
		{"n", false},
		{"y", true},
		// No answer keeps the previous one.
		{"", true},
	} {
		out := env.runWithInput(t, nil, nil, "\n"+tc.answer+"\n")
		file, ok := out.file(settingsPath)
		if !ok {
			t.Fatalf("answer %q: configure returned no settings file", tc.answer)
		}
		var settings struct {
			JudgeModel string `json:"judgeModel"`
			WebSearch  bool   `json:"webSearch"`
		}
		if err := json.Unmarshal([]byte(file["content"].(string)), &settings); err != nil {
			t.Fatal(err)
		}
		if settings.WebSearch != tc.want || settings.JudgeModel != "anthropic/claude-opus-5" {
			t.Fatalf("answer %q: settings = %+v, want webSearch %v and the edited judge kept", tc.answer, settings, tc.want)
		}
	}
}

// settingsPath is Discobox's settings file for the harness, home-relative.
const settingsPath = ".config/discobox/opencode-harness.json"

// TestLaunchAppliesTheWebSearchSetting covers the launcher's reading of the
// settings file: on turns search on for every provider, off denies it even for
// the providers opencode searches with by default, and unset leaves opencode's
// default alone.
func TestLaunchAppliesTheWebSearchSetting(t *testing.T) {
	for _, tc := range []struct {
		setting string
		want    string
	}{
		{`{"webSearch": true}`, "exa=1 parallel=1 permission="},
		{`{"webSearch": false}`, `exa= parallel= permission={"websearch":"deny"}`},
		{`{"webSearch": null}`, "exa= parallel= permission="},
		{"", "exa= parallel= permission="},
	} {
		got := runWrapper(t, "launch.sh", tc.setting, `printf 'exa=%s parallel=%s permission=%s\n' "${OPENCODE_ENABLE_EXA-}" "${OPENCODE_ENABLE_PARALLEL-}" "${OPENCODE_PERMISSION-}"`, nil)
		if got != tc.want {
			t.Errorf("setting %q: opencode ran with %q, want %q", tc.setting, got, tc.want)
		}
	}
}

// TestPromptJudgesWithTheConfiguredModel covers discobox-prompt's judge role:
// the model the settings file names, else the last /models pick — read before
// the judge is isolated from the state it lives in — else opencode's own pick,
// when opencode's configuration names no model (see the next test).
func TestPromptJudgesWithTheConfiguredModel(t *testing.T) {
	args := `for arg in "$@"; do printf '[%s]' "$arg"; done; printf '\n'`
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "opencode", "model.json"),
		[]byte(`{"recent":[{"providerID":"openai","modelID":"gpt-5.6-terra"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runWrapper(t, "prompt.sh", "", args, nil, "--model", "judge", "--no-tools", "--prompt", "is it safe"); got != "[run][--pure][is it safe]" {
		t.Errorf("no setting and no /models pick: opencode argv %s, want opencode's own pick", got)
	}
	for _, tc := range []struct {
		setting string
		want    string
	}{
		{`{"judgeModel": "anthropic/claude-opus-5"}`, "[run][--model=anthropic/claude-opus-5][--pure][is it safe]"},
		{`{"judgeModel": ""}`, "[run][--model=openai/gpt-5.6-terra][--pure][is it safe]"},
		{"", "[run][--model=openai/gpt-5.6-terra][--pure][is it safe]"},
	} {
		got := runWrapper(t, "prompt.sh", tc.setting, args, map[string]string{"XDG_STATE_HOME": state}, "--model", "judge", "--no-tools", "--prompt", "is it safe")
		if got != tc.want {
			t.Errorf("setting %q: opencode argv %s, want %s", tc.setting, got, tc.want)
		}
	}
}

// TestPromptJudgesWithOpencodesDefaultModel covers the judge following the
// model opencode itself starts with when Discobox's settings name none: the
// `model` setting in its configuration wins over the last /models pick, the
// way it does for opencode, and a .jsonc file with comments is read too.
func TestPromptJudgesWithOpencodesDefaultModel(t *testing.T) {
	args := `for arg in "$@"; do printf '[%s]' "$arg"; done; printf '\n'`
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "opencode", "model.json"),
		[]byte(`{"recent":[{"providerID":"openai","modelID":"gpt-5.6-luna"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, content, setting, want string
	}{
		{"opencode.json's model", "opencode.json", `{"model": "zai/glm-5.3"}`, "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"opencode.jsonc, with comments", "opencode.jsonc", "// my default\n{\n  // the model\n  \"model\": \"zai/glm-5.3\"\n}", "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"opencode.jsonc, as opencode writes it", "opencode.jsonc",
			"{\n  \"$schema\": \"https://opencode.ai/config.json\", // the schema\n  /* the default\n     model */\n  \"model\": \"zai/glm-5.3\",\n  \"provider\": {\"zai\": {\"options\": {\"baseURL\": \"https://api.z.ai/v1\",},},},\n}", "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"a trailing comma with a comment before its bracket", "opencode.jsonc",
			"{\n  \"model\": \"zai/glm-5.3\", // the default\n  \"small_model\": \"zai/glm-5.3-air\", /* the small one */\n}", "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"a trailing comma in an array, before a comment", "opencode.jsonc",
			"{\n  \"plugin\": [\"a\", // first\n  ],\n  \"model\": \"zai/glm-5.3\"\n}", "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"an escaped quote and a slash pair inside a string", "opencode.jsonc",
			"{\"instructions\": [\"say \\\"hi\\\" // not a comment\"], \"model\": \"zai/glm-5.3\",}", "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"a model that reads like a flag stays the model's value", "opencode.json", `{"model": "--agent=build"}`, "",
			"[run][--model=--agent=build][--pure][is it safe]"},
		{"config.json's model", "config.json", `{"model": "zai/glm-5.3"}`, "",
			"[run][--model=zai/glm-5.3][--pure][is it safe]"},
		{"a configuration naming no model falls to /models", "opencode.json", `{"theme": "tokyonight"}`, "",
			"[run][--model=openai/gpt-5.6-luna][--pure][is it safe]"},
		{"judgeModel still wins", "opencode.json", `{"model": "zai/glm-5.3"}`, `{"judgeModel": "anthropic/claude-haiku-4-5"}`,
			"[run][--model=anthropic/claude-haiku-4-5][--pure][is it safe]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			if err := os.MkdirAll(filepath.Join(config, "opencode"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(config, "opencode", tc.file), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got := runWrapper(t, "prompt.sh", tc.setting, args,
				map[string]string{"XDG_STATE_HOME": state, "XDG_CONFIG_HOME": config}, "--model", "judge", "--no-tools", "--prompt", "is it safe")
			if got != tc.want {
				t.Errorf("opencode argv %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPromptIsolatesTheToolFreeJudge covers what --no-tools shuts out: the
// caller is the agent being judged, so no configuration, instruction, plugin
// or OPENCODE_* variable it could have set may reach the judge — only the
// credentials from its auth.json, without the wellknown entries opencode would
// fetch configuration through.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	stub := `printf 'cwd-empty=%s config=%s state=%s cache=%s data=%s permission=%s project=%s claude=%s leaked=%s\n' \
		"$(ls -A | wc -l | tr -d ' ')" "$(ls -A "$XDG_CONFIG_HOME" | wc -l | tr -d ' ')" \
		"$(ls -A "$XDG_STATE_HOME" | wc -l | tr -d ' ')" "$(ls -A "$XDG_CACHE_HOME" | wc -l | tr -d ' ')" \
		"$(jq -c 'keys' "$XDG_DATA_HOME/opencode/auth.json")" "${OPENCODE_PERMISSION-}" "${OPENCODE_DISABLE_PROJECT_CONFIG-}" \
		"${OPENCODE_DISABLE_CLAUDE_CODE-}" "${OPENCODE_CONFIG_CONTENT-}${OPENCODE_AUTH_CONTENT-}"`
	// The agent's auth.json: two credentials, and a wellknown entry whose URL
	// opencode would fetch configuration from.
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte(`{
		"openai": {"type": "oauth", "access": "S", "refresh": "r", "expires": 1},
		"zai": {"type": "api", "key": "S2"},
		"https://agent.example": {"type": "wellknown", "key": "X", "token": "t"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runWrapper(t, "prompt.sh", "", stub, map[string]string{
		"XDG_DATA_HOME":           data,
		"XDG_CONFIG_HOME":         "/agent-config",
		"OPENCODE_CONFIG_CONTENT": `{"permission":{"bash":"allow"}}`,
		"OPENCODE_AUTH_CONTENT":   `{"x":{}}`,
	}, "--model", "judge", "--no-tools", "--prompt", "is it safe")
	want := `cwd-empty=0 config=0 state=0 cache=0 data=["openai","zai"] permission={"*":"deny"} project=1 claude=1 leaked=`
	if got != want {
		t.Fatalf("judge ran with\n  %s\nwant\n  %s", got, want)
	}
}

// runWrapper runs one of the image's wrappers with the settings file holding
// setting (none when empty), extra in its environment, and a stubbed opencode
// whose body is stub, and returns what the stub printed.
func runWrapper(t *testing.T, script, setting, stub string, extra map[string]string, args ...string) string {
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
	writeExecutable(t, filepath.Join(bin, "opencode"), "#!/bin/sh\n"+stub+"\n")
	scriptPath, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{scriptPath}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	env := []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENCODE_") && !strings.HasPrefix(kv, "XDG_") &&
			!strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
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
	// connect is the auth.json the stubbed interactive opencode writes,
	// standing in for what the user connects in the session.
	connect string
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
	// The stubbed opencode: a bare launch "connects" providers by writing
	// $OPENCODE_STUB_CONNECT as auth.json, and `run` answers the check.
	writeExecutable(t, filepath.Join(env.bin, "opencode"), `#!/bin/sh
case "$*" in
run*)
	printf '%s\n' "$*" >>"$HOME/../run-args"
	if [ -n "${OPENCODE_STUB_RUN_FAILS:-}" ]; then echo "Error: this model needs an opt-in" >&2; exit 1; fi
	echo "discobox-ok" ;;
"")
	mkdir -p "$HOME/.config/opencode" "$HOME/.local/share/opencode"
	echo '{"model":"openai/gpt-5.6-terra"}' >"$HOME/.config/opencode/opencode.json"
	if [ -n "${OPENCODE_STUB_CONNECT:-}" ]; then printf '%s\n' "$OPENCODE_STUB_CONNECT" >"$HOME/.local/share/opencode/auth.json"; fi ;;
*) echo "unexpected opencode $*" >&2; exit 1 ;;
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
	return e.runWithInput(t, previous, extra, "\n\n")
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
		"OPENCODE_STUB_CONNECT="+e.connect,
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
	file, ok := o.file(".local/share/opencode/auth.json")
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
