package pi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness/internal/credentialtest"
)

// stubPi stands in for pi with the storage behavior measured against pi 1.2.0:
// credentials live in ~/.pi/agent/auth.json, one entry per provider, and a
// sign-in replaces that provider's entry. A bare interactive launch "signs in"
// by merging $PI_STUB_LOGIN into auth.json and writes settings.json; and, as pi
// does before its first Copilot request, it exchanges a Copilot entry whose
// token has expired, rewriting its access token and expiry and leaving the
// GitHub token in refresh. `-p` answers the default-model check.
const stubPi = `#!/usr/bin/env node
const fs = require("fs");
const path = require("path");
const dir = path.join(process.env.HOME, ".pi", "agent");
const store = path.join(dir, "auth.json");
const args = process.argv.slice(2);
const load = () => { try { return JSON.parse(fs.readFileSync(store, "utf8")); } catch { return {}; } };
const save = (auth) => { fs.mkdirSync(dir, { recursive: true }); fs.writeFileSync(store, JSON.stringify(auth, null, 2)); };
fs.appendFileSync(path.join(process.env.HOME, "..", "pi-calls"), args.join(" ") + "\n");
if (args[0] === "-p") {
  if (process.env.PI_STUB_RUN_FAILS) { console.error("Error: this model needs an opt-in"); process.exit(1); }
  console.log("discobox-ok"); process.exit(0);
}
if (args.length === 0) {
  const auth = load();
  const copilot = auth["github-copilot"];
  if (copilot && copilot.type === "oauth" && copilot.expires <= Date.now()) {
    auth["github-copilot"] = { ...copilot, access: "real-copilot-token-from-" + copilot.refresh, expires: Date.now() + 1800000 };
  }
  if (process.env.PI_STUB_LOGIN) Object.assign(auth, JSON.parse(process.env.PI_STUB_LOGIN));
  save(auth);
  fs.writeFileSync(path.join(dir, "settings.json"), JSON.stringify({ defaultProvider: "anthropic", defaultModel: "claude-sonnet-5-5", lastChangelogVersion: "1.2.0" }));
  process.exit(0);
}
console.error("unexpected pi " + args.join(" ")); process.exit(1);
`

// signIns is what a user signs in to in the session: one of each shape the
// configure flow handles differently.
const signIns = `{
  "openrouter": {"type":"api_key","key":"real-openrouter-key","env":{"OPENROUTER_REGION":"eu"}},
  "anthropic": {"type":"oauth","access":"real-anthropic-access","refresh":"real-anthropic-refresh","expires":1900000000000},
  "xai": {"type":"oauth","access":"real-xai-access","refresh":"real-xai-refresh","expires":1900000000000},
  "github-copilot": {"type":"oauth","access":"tid=real;proxy-ep=proxy.individual.githubcopilot.com","refresh":"real-gh-token","expires":1900000000000,"availableModelIds":["gpt-5"]},
  "kimi-coding": {"type":"oauth","access":"real-kimi-access","refresh":"real-kimi-refresh","expires":1900000000000}
}`

//nolint:gosec // G101: sentinels a test renders, not credentials.
var signInSentinels = map[string]string{
	"PI_OPENROUTER_CREDENTIAL":     "S-OPENROUTER",
	"PI_ANTHROPIC_CREDENTIAL":      "S-ANTHROPIC",
	"PI_XAI_CREDENTIAL":            "S-XAI",
	"PI_GITHUB_COPILOT_CREDENTIAL": "S-GH",
	"PI_KIMI_CODING_CREDENTIAL":    "S-KIMI",
}

type configureEnv struct {
	dir    string
	home   string
	bin    string
	runDir string
	script string
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
	writeExecutable(t, env.script, strings.ReplaceAll(readFile(t, "configure.sh"), "/run/discobox/configure/", env.runDir))
	writeExecutable(t, filepath.Join(dir, "pi-image.json"), readFile(t, "image.json"))
	writeExecutable(t, filepath.Join(dir, "pi-normalize.cjs"), readFile(t, "pi-normalize.cjs"))
	// `script -q -e -c CMD /dev/null` runs CMD with a terminal; here it just
	// runs it.
	writeExecutable(t, filepath.Join(env.bin, "script"), "#!/bin/sh\nexec sh -c \"$4\"\n")
	writeExecutable(t, filepath.Join(env.bin, "pi"), stubPi)
	return env
}

type configureOutput struct {
	Files   []map[string]any `json:"files"`
	Secrets []map[string]any `json:"secrets"`
	// Printed is everything the script said.
	Printed string `json:"-"`
}

// run runs configure.sh in a fresh sandbox home with stdin input, previous as
// the seeded previous configuration, login as what the user signs in to, and
// extra in the environment, and fails the test when the script fails.
func (e *configureEnv) run(t *testing.T, input, login string, previous map[string]any, extra map[string]string) configureOutput {
	t.Helper()
	out, err := e.try(t, input, login, previous, extra)
	if err != nil {
		t.Fatalf("configure.sh: %v\n%s", err, out.Printed)
	}
	return out
}

func (e *configureEnv) try(t *testing.T, input, login string, previous map[string]any, extra map[string]string) (configureOutput, error) {
	t.Helper()
	for _, path := range []string{e.home, filepath.Join(e.dir, "pi-calls")} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
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
		"PI_STUB_LOGIN="+login,
		"NO_COLOR=1",
	)
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader(input)
	combined, err := cmd.CombinedOutput()
	out := configureOutput{Printed: string(combined)}
	if err != nil {
		return out, err
	}
	raw, err := os.ReadFile(e.runDir + "harness-configure.json")
	if err != nil {
		t.Fatalf("configure wrote no output: %v\n%s", err, combined)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

// launches counts the interactive pi sessions the script started.
func (e *configureEnv) launches(t *testing.T) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(readFile(t, filepath.Join(e.dir, "pi-calls")), "\n") {
		if line == "" {
			n++
		}
	}
	// The file ends in a newline, which reads as one empty line more.
	return n - 1
}

func (o configureOutput) file(path string) (map[string]any, bool) {
	for _, file := range o.Files {
		if file["path"] == path {
			return file, true
		}
	}
	return nil, false
}

func (o configureOutput) secret(t *testing.T, envName string) map[string]any {
	t.Helper()
	for _, secret := range o.Secrets {
		if secret["envName"] == envName {
			return secret
		}
	}
	t.Fatalf("no secret %s in %v", envName, o.Secrets)
	return nil
}

// renderAuth renders the returned auth.json the way the sandbox agent renders a
// templated harness file (sandbox-agent/terminal), and answers its entries.
func (o configureOutput) renderAuth(t *testing.T, sentinels map[string]string) (string, map[string]map[string]any) {
	t.Helper()
	file, ok := o.file(authPath)
	if !ok {
		t.Fatal("missing credential document")
	}
	rendered := credentialtest.Resolve(t, file["path"].(string), file["content"].(string), sentinels)
	entries := map[string]map[string]any{}
	if err := json.Unmarshal([]byte(rendered), &entries); err != nil {
		t.Fatalf("rendered auth.json is not JSON: %v\n%s", err, rendered)
	}
	return rendered, entries
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

func prevEnv(sentinels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range sentinels {
		out["PREV_"+name] = value
	}
	return out
}

// TestConfigureCapturesEverySignIn runs configure.sh against a stubbed pi that
// signs in the way /login leaves auth.json, and checks what the harness config
// would store and deliver — then reconfigures it, changing nothing.
func TestConfigureCapturesEverySignIn(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)

	if len(out.Secrets) != len(signInSentinels) {
		t.Fatalf("secrets = %v, want one per provider", out.Secrets)
	}
	value := func(name string) map[string]any {
		v, _ := out.secret(t, name)["value"].(map[string]any)
		return v
	}
	if s, v := out.secret(t, "PI_OPENROUTER_CREDENTIAL"), value("PI_OPENROUTER_CREDENTIAL"); s["type"] != "token" || v["token"] != "real-openrouter-key" || s["host"] != nil {
		t.Fatalf("openrouter secret = %v, want a token", s)
	}
	// Anthropic's and xAI's logins are renewed by the control plane, with the
	// clients the claude-code and opencode images are renewed with.
	if s, v := out.secret(t, "PI_ANTHROPIC_CREDENTIAL"), value("PI_ANTHROPIC_CREDENTIAL"); s["type"] != "oauth" ||
		v["token"] != "real-anthropic-access" || v["refreshToken"] != "real-anthropic-refresh" ||
		v["tokenUrl"] != "https://console.anthropic.com/v1/oauth/token" || v["clientId"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Fatalf("anthropic secret = %v", s)
	}
	if s, v := out.secret(t, "PI_XAI_CREDENTIAL"), value("PI_XAI_CREDENTIAL"); s["type"] != "oauth" ||
		v["tokenUrl"] != "https://auth.x.ai/oauth2/token" || v["tokenRequestEncoding"] != "form" {
		t.Fatalf("xai secret = %v", s)
	}
	// A Copilot login's secret is the GitHub token, bound to the host it is
	// exchanged at; the Copilot token it bought is not kept.
	if s, v := out.secret(t, "PI_GITHUB_COPILOT_CREDENTIAL"), value("PI_GITHUB_COPILOT_CREDENTIAL"); s["type"] != "token" ||
		v["token"] != "real-gh-token" || s["host"] != "api.github.com" {
		t.Fatalf("copilot secret = %v, want the GitHub token bound to api.github.com", s)
	}
	if s := out.secret(t, "PI_KIMI_CODING_CREDENTIAL"); s["type"] != "token" {
		t.Fatalf("kimi secret = %v, want a token", s)
	}
	if !strings.Contains(out.Printed, "kimi-coding's sign-in expires 2030-03-17") {
		t.Errorf("configure did not say when the Kimi login expires:\n%s", out.Printed)
	}
	if strings.Contains(out.Printed, "github-copilot's sign-in expires") {
		t.Errorf("configure warned about a Copilot token that is exchanged in the sandbox:\n%s", out.Printed)
	}

	// Rendered the way a sandbox renders it, the file carries sentinels and
	// nothing real.
	rendered, entries := out.renderAuth(t, signInSentinels)
	if strings.Contains(rendered, "real") {
		t.Fatalf("delivered auth.json holds a real credential: %s", rendered)
	}
	if e := entries["openrouter"]; e["type"] != "api_key" || e["key"] != "S-OPENROUTER" || e["env"].(map[string]any)["OPENROUTER_REGION"] != "eu" {
		t.Fatalf("delivered openrouter = %v, want the sentinel beside what pi stored with it", e)
	}
	if e := entries["anthropic"]; e["access"] != "S-ANTHROPIC" || e["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		e["expires"] != float64(4102444800000) {
		t.Fatalf("delivered anthropic = %v", e)
	}
	// pi exchanges an expired Copilot entry before its first request, so the
	// entry starts expired with the sentinel where the GitHub token was.
	if e := entries["github-copilot"]; e["refresh"] != "S-GH" || e["access"] != "discobox-copilot-token-is-exchanged-in-the-sandbox" || e["expires"] != float64(0) {
		t.Fatalf("delivered copilot = %v", e)
	}
	// A login nothing renews keeps its real expiry: pi may as well know.
	if e := entries["kimi-coding"]; e["access"] != "S-KIMI" || e["refresh"] != "discobox-refresh-happens-in-the-control-plane" || e["expires"] != float64(1900000000000) {
		t.Fatalf("delivered kimi = %v", e)
	}
	if file, ok := out.file(piSettingsPath); !ok || !strings.Contains(file["content"].(string), `"defaultModel":"claude-sonnet-5-5"`) {
		t.Fatalf("configure did not return pi's settings: %v", out.Files)
	}
	// The check is of pi's default model — what a sandbox starts on — so it
	// names none, and it uses no tools.
	calls := readFile(t, filepath.Join(env.dir, "pi-calls"))
	if !strings.Contains(calls, "-p --no-tools") || strings.Contains(calls, "--model") {
		t.Fatalf("configure did not check the default model tool-free: %s", calls)
	}

	// Reconfigure: the same harness, a user who changes nothing. Every
	// credential is seeded as its sentinel and comes back as usePrevious — the
	// Copilot one too, after the session exchanged it for a new Copilot token.
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", "", previous, prevEnv(signInSentinels))
	if len(again.Secrets) != len(signInSentinels) {
		t.Fatalf("reconfigure secrets = %v, want every one kept", again.Secrets)
	}
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true || secret["value"] != nil {
			t.Fatalf("reconfigure secret %v, want usePrevious and no value", secret)
		}
	}
	if s := again.secret(t, "PI_GITHUB_COPILOT_CREDENTIAL"); s["host"] != "api.github.com" {
		t.Fatalf("kept copilot secret = %v, want it still bound to api.github.com", s)
	}
	before, _ := out.file(authPath)
	after, _ := again.file(authPath)
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}
	if !strings.Contains(again.Printed, "Already signed in: anthropic, github-copilot, kimi-coding, openrouter, xai.") {
		t.Errorf("reconfigure did not say what it opened signed in to:\n%s", again.Printed)
	}
}

// A Copilot login to GitHub Enterprise is exchanged at that domain's API, so
// its secret is bound there.
func TestConfigureBindsAnEnterpriseCopilotLoginToItsDomain(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", `{"github-copilot":{"type":"oauth","access":"tid=real","refresh":"real-ghe-token","expires":1900000000000,"enterpriseUrl":"company.ghe.com"}}`, nil, nil)
	if s := out.secret(t, "PI_GITHUB_COPILOT_CREDENTIAL"); s["host"] != "api.company.ghe.com" {
		t.Fatalf("copilot secret = %v, want it bound to api.company.ghe.com", s)
	}
	_, entries := out.renderAuth(t, map[string]string{"PI_GITHUB_COPILOT_CREDENTIAL": "S-GHE"})
	if e := entries["github-copilot"]; e["enterpriseUrl"] != "company.ghe.com" || e["refresh"] != "S-GHE" {
		t.Fatalf("delivered copilot = %v, want its enterprise domain kept", e)
	}
}

// What cannot work in a sandbox is left out and said: a ChatGPT subscription,
// whose account pi reads from the token's claims, and a key that is a command.
func TestConfigureLeavesOutWhatCannotRunOnASentinel(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", `{
		"openai-codex": {"type":"oauth","access":"real-codex-access","refresh":"real-codex-refresh","expires":1900000000000,"accountId":"acct"},
		"anthropic": {"type":"api_key","key":"!security find-generic-password -ws anthropic"},
		"openai": {"type":"api_key","key":"real-openai-key"},
		"amazon-bedrock": {"type":"api_key","env":{"AWS_PROFILE":"work"}}
	}`, nil, nil)
	if len(out.Secrets) != 1 || out.Secrets[0]["envName"] != "PI_OPENAI_CREDENTIAL" {
		t.Fatalf("secrets = %v, want only the OpenAI API key", out.Secrets)
	}
	_, entries := out.renderAuth(t, map[string]string{"PI_OPENAI_CREDENTIAL": "S-OPENAI"})
	if len(entries) != 1 {
		t.Fatalf("delivered auth.json = %v, want only openai", entries)
	}
	for _, want := range []string{"openai-codex (a ChatGPT subscription) is not kept", "anthropic's key is a command (!security…)", "amazon-bedrock has no key or token of its own"} {
		if !strings.Contains(out.Printed, want) {
			t.Errorf("configure did not say %q:\n%s", want, out.Printed)
		}
	}
	if strings.Contains(out.Printed, "find-generic-password") {
		t.Errorf("configure printed the command itself:\n%s", out.Printed)
	}
}

// With nothing but what cannot be kept, the reason is given before the choice
// to start pi again.
func TestConfigureSaysWhyNothingWasKept(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\nn\n", `{"openai-codex":{"type":"oauth","access":"a","refresh":"r","expires":1900000000000}}`, nil, nil)
	if len(out.Secrets) != 0 {
		t.Fatalf("secrets = %v, want none", out.Secrets)
	}
	reason := strings.Index(out.Printed, "openai-codex (a ChatGPT subscription) is not kept")
	choice := strings.Index(out.Printed, "Start pi again?")
	if reason < 0 || choice < 0 || reason > choice {
		t.Fatalf("the reason did not come before the choice:\n%s", out.Printed)
	}
	if strings.Count(out.Printed, "is not kept") != 1 {
		t.Errorf("the reason was given more than once:\n%s", out.Printed)
	}
}

// A sign-in made inside a kept session replaces that credential, and only it.
func TestConfigureReplacesOneCredentialInAKeptSession(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", `{"openrouter":{"type":"api_key","key":"real-new-key"}}`, previous, prevEnv(signInSentinels))
	for _, secret := range again.Secrets {
		if secret["envName"] == "PI_OPENROUTER_CREDENTIAL" {
			if v, _ := secret["value"].(map[string]any); v["token"] != "real-new-key" || secret["usePrevious"] != nil {
				t.Fatalf("replaced key = %v, want the new key", secret)
			}
			continue
		}
		if secret["usePrevious"] != true {
			t.Fatalf("secret %v, want it kept", secret)
		}
	}
}

// A previous credential whose sentinel this sandbox was not given cannot be
// used, so it is neither offered nor kept — and the auth.json the configured
// harness delivered into this sandbox, which describes it, is not read.
func TestConfigureDropsACredentialWithNothingBehindIt(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	delivered, _ := out.renderAuth(t, signInSentinels)
	env.homeFiles = map[string]string{authPath: delivered}
	again := env.run(t, "\n", "", previous, map[string]string{"PREV_PI_OPENROUTER_CREDENTIAL": "S-OPENROUTER"})
	if len(again.Secrets) != 1 || again.Secrets[0]["envName"] != "PI_OPENROUTER_CREDENTIAL" || again.Secrets[0]["usePrevious"] != true {
		t.Fatalf("secrets = %v, want only the one with a sentinel, kept", again.Secrets)
	}
}

// A default model that does not answer is reported, and declining to start pi
// again saves every sign-in.
func TestConfigureKeepsSignInsWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\nn\n", signIns, nil, map[string]string{"PI_STUB_RUN_FAILS": "1"})
	if len(out.Secrets) != len(signInSentinels) {
		t.Fatalf("secrets = %v, want every sign-in kept", out.Secrets)
	}
	if !strings.Contains(out.Printed, "pi's default model did not answer.") || !strings.Contains(out.Printed, "this model needs an opt-in") {
		t.Errorf("the failure was not reported with pi's reason:\n%s", out.Printed)
	}
}

// Starting pi again is a person's choice, and the round's sign-ins carry into
// the next one.
func TestConfigureRetriesOnlyWhenAsked(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\ny\n\nn\n", signIns, nil, map[string]string{"PI_STUB_RUN_FAILS": "1"})
	if got := env.launches(t); got != 2 {
		t.Fatalf("pi started %d times, want 2", got)
	}
	if len(out.Secrets) != len(signInSentinels) {
		t.Fatalf("secrets = %v, want the first round's sign-ins carried through", out.Secrets)
	}
}

// Signing in to nothing is allowed after a warning, and saves no credentials.
func TestConfigureSavesNoProviderWhenAsked(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\nn\n", "", nil, nil)
	if len(out.Secrets) != 0 {
		t.Fatalf("secrets = %v, want none", out.Secrets)
	}
	if _, ok := out.file(authPath); ok {
		t.Fatal("configure returned an auth.json with nothing in it")
	}
	if !strings.Contains(out.Printed, "No provider is signed in.") {
		t.Errorf("configure did not say nothing is signed in:\n%s", out.Printed)
	}
}

// End of input at any prompt is nobody there, which fails the flow rather than
// starting a TUI at nobody or saving a choice nobody made.
func TestConfigureFailsAtTheEndOfInput(t *testing.T) {
	env := newConfigureEnv(t)
	for _, input := range []string{"", "\n"} {
		_, err := env.try(t, input, "", nil, nil)
		if err == nil {
			t.Fatalf("input %q: configure succeeded with nobody to answer", input)
		}
		if _, statErr := os.Stat(env.runDir + "harness-configure.json"); statErr == nil {
			t.Fatalf("input %q: configure wrote an output", input)
		}
	}
}

// A login nothing renews is reported when it is kept as well as when it is
// new, and one that has already expired says so.
func TestConfigureSaysWhenAKeptLoginExpires(t *testing.T) {
	env := newConfigureEnv(t)
	login := `{
  "kimi-coding": {"type":"oauth","access":"real-kimi-access","refresh":"real-kimi-refresh","expires":1900000000000},
  "meta": {"type":"oauth","access":"real-meta-key","refresh":"real-meta-identity","expires":1600000000000}
}`
	out := env.run(t, "\n", login, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", "", previous, map[string]string{
		"PREV_PI_KIMI_CODING_CREDENTIAL": "S-KIMI",
		"PREV_PI_META_CREDENTIAL":        "S-META",
	})
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true {
			t.Fatalf("secret %v, want it kept", secret)
		}
	}
	for _, want := range []string{"kimi-coding's sign-in expires 2030-03-17", "meta's sign-in expired 2020-09-13"} {
		if !strings.Contains(again.Printed, want) {
			t.Errorf("reconfigure did not say %q:\n%s", want, again.Printed)
		}
	}
}

// With no sentinel at all — every secret deleted — the auth.json the
// configured harness delivered is not taken for a sign-in: it describes
// credentials with nothing behind them.
func TestConfigureIgnoresTheDeliveredAuthWithNoSentinels(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	// Rendered with sentinels, as a sandbox renders it, but not ones this
	// sandbox was given as PREV_ values.
	delivered, _ := out.renderAuth(t, signInSentinels)
	env.homeFiles = map[string]string{authPath: delivered}
	again := env.run(t, "\nn\n", "", previous, nil)
	if len(again.Secrets) != 0 {
		t.Fatalf("secrets = %v, want none", again.Secrets)
	}
}

// OpenRouter's OAuth sign-in stores a key that does not expire, with an empty
// refresh token: delivered as it is, with no expiry notice.
func TestConfigureDeliversOpenRoutersOAuthKeyAsItIs(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", `{"openrouter":{"type":"oauth","access":"real-or-key","refresh":"","expires":9007199254740991}}`, nil, nil)
	if s := out.secret(t, "PI_OPENROUTER_CREDENTIAL"); s["type"] != "token" {
		t.Fatalf("openrouter secret = %v, want a token", s)
	}
	_, entries := out.renderAuth(t, map[string]string{"PI_OPENROUTER_CREDENTIAL": "S-OR"})
	if e := entries["openrouter"]; e["access"] != "S-OR" || e["refresh"] != "" || e["expires"] != float64(9007199254740991) {
		t.Fatalf("delivered openrouter = %v", e)
	}
	if strings.Contains(out.Printed, "sign-in expire") {
		t.Errorf("configure warned about a key that does not expire:\n%s", out.Printed)
	}
}
