package omp

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
		`process.env["PREV_" + envName]`,
		"usePrevious",
		// The credentials template is JSON, so a quote inside a template action
		// arrives at the renderer backslash-escaped and the parser rejects it.
		"{{ .secrets.${envName} }}",
		// Every retry is a person asking for one.
		"confirm_retry",
		// The check is a one-shot print, with nothing piped into it.
		"omp --print --no-session",
		"</dev/null",
		// The sign-ins are read out of omp's own store, and seeded back
		// through the same importer the launcher uses.
		"from auth_credentials where disabled_cause is null",
		"/usr/local/libexec/discobox/omp-import-credentials",
		// The first-run setup is let run here, where the sign-in is the point.
		"env -u OMP_SKIP_SETUP omp",
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
// manifest declares none, and no baseline credential file.
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
	if len(image.Harness.RunCommand) != 0 || len(image.Harness.RelaunchCommand) != 0 {
		t.Fatalf("manifest overrides the harness-run convention: run=%v relaunch=%v",
			image.Harness.RunCommand, image.Harness.RelaunchCommand)
	}
	// The policy baseline and the update check live in the image's overlay,
	// which the env names; a sandbox's credentials arrive configured, so the
	// first-run setup stays off.
	if image.Env["PI_CONFIG_FILES"] != "/etc/omp/discobox.yml" || image.Env["OMP_SKIP_SETUP"] != "1" {
		t.Fatalf("manifest env = %v, want the overlay named and setup off", image.Env)
	}
	var ports []int
	for _, port := range image.Harness.Config.Ports {
		ports = append(ports, port.Port)
		if port.Unavailable == "" {
			t.Errorf("port %d says nothing when it cannot be forwarded", port.Port)
		}
	}
	if !slices.Equal(ports, []int{1455}) {
		t.Fatalf("config ports = %v, want [1455]", ports)
	}
	if !slices.Equal(image.Harness.Config.Command, []string{"/usr/local/libexec/discobox/configure-omp"}) {
		t.Fatalf("config command = %v", image.Harness.Config.Command)
	}
}

// The overlay is the managed layer: it turns the update check off and sets
// the approval baseline, and names no extension, since an overlay's list
// would replace the user's.
func TestOverlaySetsThePolicyBaseline(t *testing.T) {
	raw, err := os.ReadFile("managed-config.yml")
	if err != nil {
		t.Fatal(err)
	}
	overlay := string(raw)
	for _, want := range []string{"checkUpdate: false", "approvalMode: yolo"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("overlay does not set %s", want)
		}
	}
	if strings.Contains(overlay, "extensions:") {
		t.Error("overlay names extensions, which replaces the user's list")
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "managed-config.yml /etc/omp/discobox.yml") {
		t.Error("Dockerfile does not install the overlay where the manifest's env names it")
	}
}

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). omp joins positional messages itself but reads one
// beginning with @ as a file, and one naming a subcommand as that command, so
// the joined prompt is one argument after `--`. The image's hook extension is
// named on every launch.
func TestLaunchJoinsThePromptWords(t *testing.T) {
	baseline := []string{"--hook", "/usr/local/libexec/discobox/omp-hook-extension.js"}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, append(slices.Clone(baseline), "--", "fix the failing tests")},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, append(slices.Clone(baseline), "--", "fix the failing tests")},
		{"a prompt that names a subcommand stays a prompt", []string{"commit", "the", "fix"}, append(slices.Clone(baseline), "--", "commit the fix")},
		{"no prompt passes none", nil, baseline},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, append(slices.Clone(baseline), "--continue")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "omp", nil, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("omp argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestLaunchImportsTheDeliveredSignIns covers the launcher's other job: the
// delivered OAuth file goes through the importer before omp starts, and its
// absence costs nothing.
func TestLaunchImportsTheDeliveredSignIns(t *testing.T) {
	launcher, err := os.ReadFile("launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`AUTH_FILE="$HOME/.omp/agent/discobox-auth.json"`,
		`sh /usr/local/libexec/discobox/omp-import-credentials "$AUTH_FILE" || true`,
	} {
		if !strings.Contains(string(launcher), want) {
			t.Errorf("launcher is missing %q", want)
		}
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "import.sh /usr/local/libexec/discobox/omp-import-credentials") {
		t.Error("Dockerfile does not install the importer where the launcher and configure flow run it")
	}
}

// TestImportSkipsWhatIsAlreadyActive runs import.sh against a stubbed omp: an
// entry whose access token is the active credential is left alone, one that
// differs replaces the provider's rows, and a provider omp refuses is reported
// without failing.
func TestImportSkipsWhatIsAlreadyActive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "jq", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run the importer with", tool)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stubbed omp: `token` answers from $OMP_STUB_ACTIVE (provider=token
	// pairs), `auth-broker import` refuses $OMP_STUB_REFUSES, and every
	// call is logged.
	writeExecutable(t, filepath.Join(bin, "omp"), `#!/bin/sh
printf '%s\n' "$*" >>"$OMP_STUB_LOG"
case "$1 $2" in
"token "*)
	for pair in $OMP_STUB_ACTIVE; do
		case "$pair" in "$2="*) printf '%s\n' "${pair#*=}"; exit 0 ;; esac
	done
	echo "No active credential found for provider \"$2\"." >&2; exit 1 ;;
"auth-broker import")
	case " $OMP_STUB_REFUSES " in *" $5 "*) echo "refused" >&2; exit 1 ;; esac
	cat "$3" >>"$OMP_STUB_LOG"; echo ;;
"auth-broker logout") ;;
*) echo "unexpected omp $*" >&2; exit 1 ;;
esac
`)
	auth := filepath.Join(dir, "discobox-auth.json")
	if err := os.WriteFile(auth, []byte(`{
		"anthropic": {"access_token": "S-ANTHROPIC", "refresh_token": "placeholder", "expired": "2100-01-01T00:00:00.000Z"},
		"openai-codex": {"access_token": "S-CODEX", "refresh_token": "placeholder", "expired": "2100-01-01T00:00:00.000Z", "account_id": "acct-1"},
		"kimi-code": {"access_token": "S-KIMI", "refresh_token": "placeholder", "expired": "2100-01-01T00:00:00.000Z"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	script, err := filepath.Abs("import.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", script, auth) //nolint:gosec // The importer under test.
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OMP_STUB_LOG="+log,
		// anthropic is already the delivered sentinel; codex holds an account
		// the user signed in to by hand; kimi is refused by omp.
		"OMP_STUB_ACTIVE=anthropic=S-ANTHROPIC openai-codex=by-hand",
		"OMP_STUB_REFUSES=kimi-code",
	)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("import.sh: %v\n%s", err, stderr)
	}
	if !strings.Contains(string(stderr), "could not import the configured kimi-code credential") {
		t.Errorf("a refused import was not reported: %s", stderr)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(calls)
	if strings.Contains(got, "auth-broker logout anthropic") || strings.Contains(got, "--provider anthropic") {
		t.Errorf("an already active sentinel was imported again:\n%s", got)
	}
	if !strings.Contains(got, "auth-broker logout openai-codex") || !strings.Contains(got, "--provider openai-codex") ||
		!strings.Contains(got, `"account_id": "acct-1"`) {
		t.Errorf("a differing credential was not signed out and replaced with the delivered entry:\n%s", got)
	}
	if strings.Contains(got, "S-ANTHROPIC\n") && strings.Count(got, "S-ANTHROPIC") > 1 {
		t.Errorf("the active sentinel was re-imported:\n%s", got)
	}
}

// signedInRows is omp's store as /login leaves it for five providers, one of
// each shape the configure flow handles differently.
const signedInRows = `[
  {"provider": "anthropic", "credential_type": "oauth", "data": {"refresh": "real-anthropic-refresh", "access": "real-anthropic-access", "expires": 1900000000000, "email": "me@example.com"}},
  {"provider": "openai-codex", "credential_type": "oauth", "data": {"refresh": "real-openai-refresh", "access": "real-openai-access", "expires": 1900000000000, "accountId": "acct-1"}},
  {"provider": "github-copilot", "credential_type": "oauth", "data": {"refresh": "real-gh-token", "access": "real-copilot-token", "expires": 1700000000000}},
  {"provider": "kimi-code", "credential_type": "oauth", "data": {"refresh": "real-kimi-refresh", "access": "real-kimi-access", "expires": 1900000000000}},
  {"provider": "zai", "credential_type": "api_key", "data": {"key": "real-zai-key"}}
]`

// TestConfigureCapturesEverySignedInProvider runs configure.sh against a
// stubbed omp that "signs in" providers the way /login leaves them, and checks
// what the harness config would store and deliver.
func TestConfigureCapturesEverySignedInProvider(t *testing.T) {
	env := newConfigureEnv(t)
	env.login = signedInRows
	env.homeFiles = map[string]string{
		// The user's own custom provider, which the returned file keeps.
		".omp/agent/models.yml": "providers:\n  my-gateway:\n    baseUrl: https://gateway.example.com/v1\n    api: openai-completions\n    apiKey: MY_GATEWAY_API_KEY\n",
	}
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
	if v := value("OMP_ANTHROPIC_CREDENTIAL"); secrets["OMP_ANTHROPIC_CREDENTIAL"]["type"] != "oauth" ||
		v["token"] != "real-anthropic-access" || v["refreshToken"] != "real-anthropic-refresh" ||
		v["tokenUrl"] != "https://platform.claude.com/v1/oauth/token" {
		t.Fatalf("anthropic secret = %v", secrets["OMP_ANTHROPIC_CREDENTIAL"])
	}
	if v := value("OMP_OPENAI_CODEX_CREDENTIAL"); secrets["OMP_OPENAI_CODEX_CREDENTIAL"]["type"] != "oauth" ||
		v["tokenUrl"] != "https://auth.openai.com/oauth/token" {
		t.Fatalf("openai-codex secret = %v", secrets["OMP_OPENAI_CODEX_CREDENTIAL"])
	}
	if v := value("OMP_GITHUB_COPILOT_CREDENTIAL"); secrets["OMP_GITHUB_COPILOT_CREDENTIAL"]["type"] != "token" || v["token"] != "real-gh-token" {
		t.Fatalf("copilot secret = %v, want a token holding the GitHub token", secrets["OMP_GITHUB_COPILOT_CREDENTIAL"])
	}
	if v := value("OMP_KIMI_CODE_CREDENTIAL"); secrets["OMP_KIMI_CODE_CREDENTIAL"]["type"] != "token" || v["token"] != "real-kimi-access" {
		t.Fatalf("kimi secret = %v, want a token", secrets["OMP_KIMI_CODE_CREDENTIAL"])
	}
	if v := value("OMP_ZAI_CREDENTIAL"); secrets["OMP_ZAI_CREDENTIAL"]["type"] != "token" || v["token"] != "real-zai-key" {
		t.Fatalf("zai secret = %v, want a token", secrets["OMP_ZAI_CREDENTIAL"])
	}

	// Rendered the way a sandbox renders it, the OAuth file carries sentinels
	// and nothing real, in the importer's shape.
	rendered := out.renderAuth(t, map[string]string{ //nolint:gosec // Sentinels a test renders into the file, not credentials.
		"OMP_ANTHROPIC_CREDENTIAL":      "S-ANTHROPIC",
		"OMP_OPENAI_CODEX_CREDENTIAL":   "S-CODEX",
		"OMP_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"OMP_KIMI_CODE_CREDENTIAL":      "S-KIMI",
	})
	if strings.Contains(rendered, "real-") {
		t.Fatalf("delivered credentials hold a real value: %s", rendered)
	}
	var auth map[string]map[string]any
	if err := json.Unmarshal([]byte(rendered), &auth); err != nil {
		t.Fatalf("rendered file is not JSON: %v\n%s", err, rendered)
	}
	if a := auth["anthropic"]; a["access_token"] != "S-ANTHROPIC" || a["refresh_token"] != "discobox-refresh-happens-in-the-control-plane" ||
		a["expired"] != "2100-01-01T00:00:00.000Z" || a["email"] != "me@example.com" {
		t.Fatalf("delivered anthropic entry = %v", a)
	}
	if c := auth["openai-codex"]; c["access_token"] != "S-CODEX" || c["account_id"] != "acct-1" {
		t.Fatalf("delivered openai-codex entry = %v, want the account carried with the sentinel", c)
	}
	if gh := auth["github-copilot"]; gh["access_token"] != "S-GH" || gh["refresh_token"] != "S-GH" || gh["expired"] != "1970-01-01T00:00:00.000Z" {
		t.Fatalf("delivered copilot entry = %v, want the sentinel in both token fields, already expired", gh)
	}
	if _, ok := auth["zai"]; ok {
		t.Fatalf("delivered file = %v, want no entry for an API key, which models.yml delivers", auth)
	}
	// The API key reaches omp through models.yml, which names the variable
	// the secret is exported as, beside the user's own provider.
	models, ok := out.file(".omp/agent/models.yml")
	if !ok {
		t.Fatal("configure did not return models.yml")
	}
	if models["template"] == true {
		t.Fatal("models.yml is a template; it names a variable, and must not carry the sentinel")
	}
	var parsed struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal([]byte(models["content"].(string)), &parsed); err != nil {
		t.Fatalf("models.yml is not the JSON this script writes: %v\n%s", err, models["content"])
	}
	if parsed.Providers["zai"]["apiKey"] != "OMP_ZAI_CREDENTIAL" {
		t.Fatalf("models.yml providers = %v, want zai's key named by its variable", parsed.Providers)
	}
	if gw := parsed.Providers["my-gateway"]; gw["apiKey"] != "MY_GATEWAY_API_KEY" || gw["baseUrl"] != "https://gateway.example.com/v1" {
		t.Fatalf("models.yml providers = %v, want the user's own provider kept", parsed.Providers)
	}
	if _, ok := out.file(".omp/agent/config.yml"); !ok {
		t.Fatal("configure did not return the settings the user left")
	}
	printArgs, err := os.ReadFile(filepath.Join(env.dir, "print-args"))
	if err != nil {
		t.Fatalf("configure never checked the default model: %v", err)
	}
	if strings.Contains(string(printArgs), "--model") {
		t.Fatalf("configure checked a chosen model rather than the default: %s", printArgs)
	}

	// Reconfigure: the same harness, a user who changes nothing. Every
	// sign-in is seeded as its sentinel — the OAuth ones imported into the
	// store, the API key re-pointed in models.yml — and comes back as
	// usePrevious.
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	env.login = ""
	env.homeFiles = map[string]string{}
	for _, file := range out.Files {
		env.homeFiles[file["path"].(string)] = file["content"].(string)
	}
	again := env.run(t, previous, map[string]string{ //nolint:gosec // Sentinels a test seeds, not credentials.
		"PREV_OMP_ANTHROPIC_CREDENTIAL":      "S-ANTHROPIC",
		"PREV_OMP_OPENAI_CODEX_CREDENTIAL":   "S-CODEX",
		"PREV_OMP_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PREV_OMP_KIMI_CODE_CREDENTIAL":      "S-KIMI",
		"PREV_OMP_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if len(again.Secrets) != 5 {
		t.Fatalf("reconfigure secrets = %v, want the five kept", again.Secrets)
	}
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true || secret["value"] != nil {
			t.Fatalf("reconfigure secret %v, want usePrevious and no value", secret)
		}
	}
	before, _ := out.file(".omp/agent/discobox-auth.json")
	after, _ := again.file(".omp/agent/discobox-auth.json")
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}
	modelsAgain, _ := again.file(".omp/agent/models.yml")
	if !strings.Contains(modelsAgain["content"].(string), `"apiKey": "OMP_ZAI_CREDENTIAL"`) || strings.Contains(modelsAgain["content"].(string), "PREV_") {
		t.Fatalf("reconfigured models.yml = %s, want the key pointed back at its own variable", modelsAgain["content"])
	}
	// The seed signed the session in: the stub saw the sentinels imported
	// before it started, and the key re-pointed at its PREV_ variable.
	seededRows, err := os.ReadFile(filepath.Join(env.dir, "seeded-rows"))
	if err != nil || !strings.Contains(string(seededRows), "S-ANTHROPIC") || !strings.Contains(string(seededRows), "S-GH") {
		t.Fatalf("the previous sign-ins were not imported before omp started: %s (%v)", seededRows, err)
	}
	seededModels, err := os.ReadFile(filepath.Join(env.dir, "seeded-models"))
	if err != nil || !strings.Contains(string(seededModels), `"apiKey": "PREV_OMP_ZAI_CREDENTIAL"`) {
		t.Fatalf("the previous API key was not re-pointed at its PREV_ variable before omp started: %s (%v)", seededModels, err)
	}
}

// TestConfigureDropsASignInWhoseSecretIsGone covers a reconfigure after a
// secret was deleted: nothing seeds it, the user signs in to nothing new, and
// the provider is gone from what is returned — including its models.yml
// entry, which would otherwise hand omp a variable's name as a key.
func TestConfigureDropsASignInWhoseSecretIsGone(t *testing.T) {
	env := newConfigureEnv(t)
	env.homeFiles = map[string]string{
		".omp/agent/models.yml":             `{"providers": {"zai": {"apiKey": "OMP_ZAI_CREDENTIAL"}, "groq": {"apiKey": "OMP_GROQ_CREDENTIAL"}}}`,
		".omp/agent/discobox-auth.json":     `{"anthropic": {"access_token": "{{ .secrets.OMP_ANTHROPIC_CREDENTIAL }}", "refresh_token": "x", "expired": "2100-01-01T00:00:00.000Z"}}`,
		".config/discobox/omp-harness.json": `{"judgeModel": "zai/glm-5.3"}`,
	}
	previous := map[string]any{
		"files": []map[string]any{
			{"path": ".omp/agent/models.yml", "content": env.homeFiles[".omp/agent/models.yml"]},
			{"path": ".omp/agent/discobox-auth.json", "template": true, "content": env.homeFiles[".omp/agent/discobox-auth.json"]},
		},
		"secrets": []map[string]any{
			{"envName": "OMP_ZAI_CREDENTIAL", "type": "token"},
			{"envName": "OMP_GROQ_CREDENTIAL", "type": "token"},
			{"envName": "OMP_ANTHROPIC_CREDENTIAL", "type": "oauth"},
		},
	}
	// Only zai's secret still exists.
	out := env.runWithInput(t, previous, map[string]string{"PREV_OMP_ZAI_CREDENTIAL": "S-ZAI"}, "\n")
	var names []string
	for _, secret := range out.Secrets {
		names = append(names, secret["envName"].(string))
	}
	if !slices.Equal(names, []string{"OMP_ZAI_CREDENTIAL"}) {
		t.Fatalf("secrets = %v, want only the one whose secret exists, kept", names)
	}
	if _, ok := out.file(".omp/agent/discobox-auth.json"); ok {
		t.Fatal("configure returned an OAuth file with no sign-in behind it")
	}
	models, _ := out.file(".omp/agent/models.yml")
	if content, _ := models["content"].(string); !strings.Contains(content, "OMP_ZAI_CREDENTIAL") || strings.Contains(content, "GROQ") {
		t.Fatalf("models.yml = %s, want groq's dangling entry gone", content)
	}
	settings, _ := out.file(settingsPath)
	if content, _ := settings["content"].(string); !strings.Contains(content, "zai/glm-5.3") {
		t.Fatalf("settings = %s, want the edited judge kept", content)
	}
}

// TestConfigureKeepsProvidersWhenTheDefaultModelFails covers a default model
// that does not answer — a plan or region restriction, not a bad credential.
// It is reported, and declining to start omp again saves every provider.
func TestConfigureKeepsProvidersWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	env.login = `[{"provider": "opencode-go", "credential_type": "api_key", "data": {"key": "real-go-key"}}, {"provider": "zai", "credential_type": "api_key", "data": {"key": "real-zai-key"}}]`
	out := env.runWithInput(t, nil, map[string]string{"OMP_STUB_PRINT_FAILS": "1"}, "\nn\n")
	var names []string
	for _, secret := range out.Secrets {
		names = append(names, secret["envName"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"OMP_OPENCODE_GO_CREDENTIAL", "OMP_ZAI_CREDENTIAL"}) {
		t.Fatalf("secrets = %v, want both providers kept", names)
	}
}

// settingsPath is Discobox's settings file for the harness, home-relative.
const settingsPath = ".config/discobox/omp-harness.json"

// TestPromptJudgesWithTheConfiguredModel covers discobox-prompt's judge role:
// the model the settings file names, else omp's own default role — asked of
// omp before the judge is isolated from the configuration it lives in — else
// omp's own pick, when neither names one.
func TestPromptJudgesWithTheConfiguredModel(t *testing.T) {
	args := `case "$1 $2" in "config get") printf '%s\n' "${OMP_STUB_DEFAULT-}"; exit 0 ;; esac; for arg in "$@"; do printf '[%s]' "$arg"; done; printf '\n'`
	isolated := "[--no-tools][--no-extensions][--no-skills][--no-rules][--no-lsp][--no-pty]"
	for _, tc := range []struct {
		name, setting, ompDefault, want string
	}{
		{"nothing named leaves the model to omp", "", "",
			"[--print][--no-session][--no-title][--thinking][off][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"judgeModel wins", `{"judgeModel": "anthropic/claude-opus-5"}`, "openai-codex/gpt-5.6",
			"[--print][--no-session][--no-title][--thinking][off][--model=anthropic/claude-opus-5][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"omp's default role", `{"judgeModel": ""}`, "openai-codex/gpt-5.6",
			"[--print][--no-session][--no-title][--thinking][off][--model=openai-codex/gpt-5.6][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"a default omp prints quoted", "", `"openai-codex/gpt-5.6"`,
			"[--print][--no-session][--no-title][--thinking][off][--model=openai-codex/gpt-5.6][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"an unset role is not a model", "", "null",
			"[--print][--no-session][--no-title][--thinking][off][--system-prompt][decide]" + isolated + "[--][is it safe]"},
		{"a message is not a model", "", "Setting not found: modelRoles.default",
			"[--print][--no-session][--no-title][--thinking][off][--system-prompt][decide]" + isolated + "[--][is it safe]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runWrapper(t, "prompt.sh", tc.setting, args, map[string]string{"OMP_STUB_DEFAULT": tc.ompDefault},
				"--model", "judge", "--no-tools", "--system", "decide", "--prompt", "is it safe")
			if got != tc.want {
				t.Errorf("omp argv %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPromptIsolatesTheToolFreeJudge covers what --no-tools shuts out: the
// caller is the agent being judged, so no configuration, instruction,
// extension or PI_*/OMP_* variable it could have set may reach the judge —
// only its credentials: the store, models.yml, and the secrets the sandbox
// exports, under the image's own overlay.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3 to make the store with")
	}
	stub := `case "$1 $2" in "config get") exit 1 ;; esac
	printf 'cwd-empty=%s agent=%s home=%s overlay=%s setup=%s leaked=%s secret=%s stdin=%s rows=%s\n' \
		"$(ls -A | wc -l | tr -d ' ')" "$(ls -A "$PI_CODING_AGENT_DIR" | sort | tr '\n' ' ')" "$(ls -A "$HOME" | wc -l | tr -d ' ')" \
		"${PI_CONFIG_FILES-}" "${OMP_SKIP_SETUP-}" "${PI_SLOW_MODEL-}${OMP_AUTH_BROKER_URL-}${PI_CONFIG_DIR-}" "${OMP_ZAI_CREDENTIAL-}" "$(cat)" \
		"$(sqlite3 "$PI_CODING_AGENT_DIR/agent.db" 'select provider from auth_credentials' | tr '\n' ' ')"`
	agent := t.TempDir()
	db := exec.CommandContext(t.Context(), "sqlite3", filepath.Join(agent, "agent.db"), //nolint:gosec // The store the judge is given a snapshot of.
		"create table auth_credentials(id integer primary key, provider text, credential_type text, data text, disabled_cause text); insert into auth_credentials(provider, credential_type, data) values ('anthropic', 'oauth', '{}');")
	if out, err := db.CombinedOutput(); err != nil {
		t.Fatalf("make the store: %v: %s", err, out)
	}
	for name, content := range map[string]string{
		"models.yml": `{"providers": {"zai": {"apiKey": "OMP_ZAI_CREDENTIAL"}}}`,
		"config.yml": "extensions:\n  - /tmp/evil.ts\n",
		".env":       "PI_SLOW_MODEL=evil/model\n",
	} {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := runWrapperWithStdin(t, "prompt.sh", "", stub, map[string]string{
		"PI_CODING_AGENT_DIR": agent,
		"PI_CONFIG_FILES":     "/etc/omp/discobox.yml",
		"OMP_SKIP_SETUP":      "1",
		"PI_SLOW_MODEL":       "evil/model",
		"OMP_AUTH_BROKER_URL": "http://evil.example",
		"PI_CONFIG_DIR":       ".evil",
		"OMP_ZAI_CREDENTIAL":  "S-ZAI",
	}, "planted on stdin", "--model", "judge", "--no-tools", "--prompt", "is it safe")
	want := `cwd-empty=0 agent=agent.db models.yml  home=0 overlay=/etc/omp/discobox.yml setup=1 leaked= secret=S-ZAI stdin= rows=anthropic`
	if got != want {
		t.Fatalf("judge ran with\n  %s\nwant\n  %s", got, want)
	}
}

// runWrapper runs one of the image's wrappers with the settings file holding
// setting (none when empty), extra in its environment, and a stubbed omp whose
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
	for _, tool := range []string{"sh", "jq", "timeout"} {
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
	writeExecutable(t, filepath.Join(bin, "omp"), "#!/bin/sh\n"+stub+"\n")
	scriptPath, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{scriptPath}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	env := []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PI_") && !strings.HasPrefix(kv, "OMP_") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "PATH=") {
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
	// login is the rows the stubbed interactive omp writes into its store,
	// standing in for what the user signs in to in the session.
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
	for _, tool := range []string{"sh", "node", "jq", "sqlite3", "timeout", "grep", "tail", "mktemp", "cat"} {
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
	importer, err := filepath.Abs("import.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(raw), "/run/discobox/configure/", env.runDir)
	script = strings.ReplaceAll(script, "IMPORT=/usr/local/libexec/discobox/omp-import-credentials", "IMPORT="+importer)
	writeExecutable(t, env.script, script)
	// `script -q -e -c CMD /dev/null` runs CMD with a terminal; here it just
	// runs it. `env -u OMP_SKIP_SETUP omp` is the command.
	writeExecutable(t, filepath.Join(env.bin, "script"), "#!/bin/sh\nexec sh -c \"$4\"\n")
	// The stubbed omp keeps a store shaped like omp's own, in the one table
	// configure.sh reads: a bare launch "signs in" by inserting
	// $OMP_STUB_LOGIN's rows (and records what the seed left, for the test to
	// check), `--print` answers the check, and `token`, `auth-broker logout`
	// and `auth-broker import` do what the importer expects of them.
	writeExecutable(t, filepath.Join(env.bin, "omp"), `#!/bin/sh
DB="$HOME/.omp/agent/agent.db"
mkdir -p "$HOME/.omp/agent"
sqlite3 "$DB" "create table if not exists auth_credentials(id integer primary key autoincrement, provider text not null, credential_type text not null, data text not null, disabled_cause text default null)"
case "$*" in
"")
	sqlite3 -json "$DB" "select provider, data from auth_credentials where disabled_cause is null" >"$HOME/../seeded-rows"
	cat "$HOME/.omp/agent/models.yml" >"$HOME/../seeded-models" 2>/dev/null || true
	printf 'modelRoles:\n  default: openai-codex/gpt-5.6-terra\n' >"$HOME/.omp/agent/config.yml"
	if [ -n "${OMP_STUB_LOGIN:-}" ]; then
		printf '%s' "$OMP_STUB_LOGIN" | jq -r '.[] | [.provider, .credential_type, (.data | tojson)] | @tsv' | while IFS="$(printf '\t')" read -r provider kind data; do
			sqlite3 "$DB" "insert into auth_credentials(provider, credential_type, data) values ('$provider', '$kind', '$data')"
		done
	fi ;;
--print*)
	printf '%s\n' "$*" >>"$HOME/../print-args"
	if [ -n "${OMP_STUB_PRINT_FAILS:-}" ]; then echo "Error: this model needs an opt-in" >&2; exit 1; fi
	echo "discobox-ok" ;;
"token "*)
	value=$(sqlite3 "$DB" "select json_extract(data, '$.access') from auth_credentials where provider = '$2' and disabled_cause is null order by id limit 1")
	if [ -z "$value" ]; then echo "No active credential found for provider \"$2\"." >&2; exit 1; fi
	printf '%s\n' "$value" ;;
"auth-broker logout "*)
	sqlite3 "$DB" "update auth_credentials set disabled_cause = 'logged out by user' where provider = '$3'" ;;
"auth-broker import "*)
	data=$(jq -c '{access: .access_token, refresh: .refresh_token, expires: (.expired | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 * 1000)} + (if .account_id then {accountId: .account_id} else {} end) + (if .email then {email: .email} else {} end)' "$3")
	sqlite3 "$DB" "insert into auth_credentials(provider, credential_type, data) values ('$5', 'oauth', '$data')" ;;
*) echo "unexpected omp $*" >&2; exit 1 ;;
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
		"OMP_STUB_LOGIN="+e.login,
		"OMP_SKIP_SETUP=1",
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

// renderAuth renders the returned credential file the way the sandbox agent
// renders a templated harness file (sandbox-agent/terminal).
func (o configureOutput) renderAuth(t *testing.T, sentinels map[string]string) string {
	t.Helper()
	file, ok := o.file(".omp/agent/discobox-auth.json")
	if !ok || file["template"] != true {
		t.Fatalf("no templated discobox-auth.json in %v", o.Files)
	}
	tmpl, err := template.New("auth").Funcs(template.FuncMap{"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		return string(raw), err
	}}).Option("missingkey=zero").Parse(file["content"].(string))
	if err != nil {
		t.Fatalf("discobox-auth.json template does not parse: %v", err)
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
	// omp facts Claude Code has no word for. Each is still recorded and still
	// waitable under omp's own name (ADR 0146 §3).
	noCanonicalName := []string{
		"session_switch",
		"agent_start",
		"agent_end",
		"turn_start",
		"turn_end",
		"tool_approval_resolved",
		"credential_disabled",
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

// The mapping table must not name an omp event the extension never publishes:
// a translation for an event nothing emits is a claim about omp that nothing
// tests.
func TestMappedEventsAreAllPublished(t *testing.T) {
	published := publishedExtensionEvents(t)
	for _, event := range []string{
		"session_start", "session_shutdown", "before_agent_start", "session_stop",
		"tool_call", "tool_result", "tool_approval_requested", "session_before_compact", "session_compact",
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
		"context", "before_provider_request", "after_provider_response", "input", "user_bash", "user_python",
	} {
		if slices.Contains(published, event) {
			t.Errorf("extension publishes %s, which is excluded on purpose", event)
		}
	}
}

// Every hook this image publishes names omp as its provider, which is what the
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
	const installed = "/usr/local/libexec/discobox/omp-hook-extension.js"
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

// What the extension does, not what it says: it runs against a synthetic omp,
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
		"session_start", "before_agent_start", "agent_start", "turn_start", "tool_call", "tool_approval_requested",
		"tool_result", "turn_end", "agent_end", "session_stop", "session_shutdown",
	}
	if !slices.Equal(events, want) {
		t.Fatalf("published\n got %v\nwant %v", events, want)
	}
	byEvent := map[string]map[string]any{}
	for _, p := range published {
		byEvent[p.Event] = p.Payload
	}
	if prompt := byEvent["before_agent_start"]; prompt["prompt"] != "fix the tests" || prompt["images"] != nil || prompt["systemPrompt"] != nil {
		t.Errorf("before_agent_start payload = %v, want the prompt and not the images or system prompt", prompt)
	}
	if call := byEvent["tool_call"]; call["toolName"] != "bash" || call["toolCallId"] != "call_1" {
		t.Errorf("tool_call payload = %v", call)
	} else if input, _ := call["input"].(map[string]any); input["command"] != "ls -la" {
		t.Errorf("tool_call payload = %v, want the tool's input", call)
	}
	if asked := byEvent["tool_approval_requested"]; asked["toolName"] != "bash" || asked["approvalMode"] != "yolo" {
		t.Errorf("tool_approval_requested payload = %v", asked)
	}
	if result := byEvent["tool_result"]; result["content"] != nil || result["details"] != nil || result["isError"] != false {
		t.Errorf("tool_result payload = %v, want the outcome and not the content", result)
	}
	if stop := byEvent["session_stop"]; stop["messages"] != nil || stop["last_assistant_message"] != nil ||
		stop["turn_id"] != "t1" || stop["stop_hook_active"] != false {
		t.Errorf("session_stop payload = %v, want the turn and not the transcript", stop)
	}
}
