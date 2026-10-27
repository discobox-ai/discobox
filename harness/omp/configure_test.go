package omp

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
)

// stubOmp stands in for omp with the storage behavior measured against omp
// 18.11.0: credentials live in one store (agent.db, here a JSON file of the
// same name), `auth export` prints them as {version, credentials}, and `auth
// import` merges a document into them — an API key replacing the provider's
// API key, an OAuth login replacing the same account's. A bare interactive
// launch "signs in" by importing $OMP_STUB_LOGIN and writes config.yml,
// `config get modelRoles` reads the default role back from it, and `-p`
// answers the default-model check.
const stubOmp = `#!/usr/bin/env node
const fs = require("fs");
const path = require("path");
const store = path.join(process.env.HOME, ".omp", "agent", "agent.db");
const args = process.argv.slice(2);
const load = () => { try { return JSON.parse(fs.readFileSync(store, "utf8")); } catch { return []; } };
const save = (entries) => { fs.mkdirSync(path.dirname(store), { recursive: true }); fs.writeFileSync(store, JSON.stringify(entries)); };
const merge = (doc) => {
  let entries = load();
  for (const incoming of doc.credentials) {
    entries = entries.filter((e) => !(e.provider === incoming.provider && e.credential.type === incoming.credential.type &&
      (incoming.credential.type === "api_key" || e.credential.email === incoming.credential.email)));
    entries.push(incoming);
  }
  save(entries);
};
fs.appendFileSync(process.env.STUB_OMP_CALLS, args.join(" ") + "\n");
if (args[0] === "auth" && args[1] === "import") { merge(JSON.parse(fs.readFileSync(args[2], "utf8"))); process.exit(0); }
if (args[0] === "auth" && args[1] === "export") { process.stdout.write(JSON.stringify({ version: 1, credentials: load() })); process.exit(0); }
if (args.join(" ") === "config get modelRoles --json") {
  let config = "";
  try { config = fs.readFileSync(path.join(process.env.HOME, ".omp", "agent", "config.yml"), "utf8"); } catch {}
  const role = /default: (\S+)/.exec(config);
  console.log(JSON.stringify({ key: "modelRoles", value: role ? { default: role[1] } : {} })); process.exit(0);
}
if (args[0] === "-p") {
  if (process.env.OMP_STUB_RUN_FAILS) { console.error("Error: this model needs an opt-in"); process.exit(1); }
  console.log("discobox-ok"); process.exit(0);
}
if (args[0] === "--allow-home") {
  fs.mkdirSync(path.join(process.env.HOME, ".omp", "agent"), { recursive: true });
  fs.writeFileSync(path.join(process.env.HOME, ".omp", "agent", "config.yml"), "modelRoles:\n  default: anthropic/claude-sonnet-5-5\n");
  if (process.env.OMP_STUB_LOGIN) merge(JSON.parse(process.env.OMP_STUB_LOGIN));
  process.exit(0);
}
console.error("unexpected omp " + args.join(" ")); process.exit(1);
`

// signIns is what a user signs in to in the session: one of each shape the
// configure flow handles differently, and a second account of one provider.
const signIns = `{"version":1,"credentials":[
  {"provider":"openrouter","credential":{"type":"api_key","key":"real-openrouter-key"}},
  {"provider":"anthropic","credential":{"type":"oauth","access":"real-anthropic-access","refresh":"real-anthropic-refresh","expires":1900000000000,"accountId":"acct-1","email":"me@example.com"}},
  {"provider":"anthropic","credential":{"type":"oauth","access":"real-anthropic-access-2","refresh":"real-anthropic-refresh-2","expires":1900000000000,"accountId":"acct-2","email":"work@example.com"}},
  {"provider":"openai-codex","credential":{"type":"oauth","access":"real-codex-access","refresh":"real-codex-refresh","expires":1900000000000}},
  {"provider":"github-copilot","credential":{"type":"oauth","access":"real-gh-token","refresh":"real-gh-token","expires":4102444800000}},
  {"provider":"google-gemini-cli","credential":{"type":"oauth","access":"real-gemini-access","refresh":"real-gemini-refresh","expires":1900000000000,"projectId":"p-1"}}
]}`

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
	for _, tool := range []string{"sh", "node", "jq", "timeout", "grep", "tail", "mktemp", "cat"} {
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
	writeExecutable(t, filepath.Join(dir, "omp-image.json"), readFile(t, "image.json"))
	writeExecutable(t, filepath.Join(dir, "omp-normalize.cjs"), readFile(t, "omp-normalize.cjs"))
	// `script -q -e -c CMD /dev/null` runs CMD with a terminal; here it just
	// runs it.
	writeExecutable(t, filepath.Join(env.bin, "script"), "#!/bin/sh\nexec sh -c \"$4\"\n")
	writeExecutable(t, filepath.Join(env.bin, "omp"), stubOmp)
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
// extra in the environment. It answers the output, or fails the test when the
// script fails unless wantFail.
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
	for _, path := range []string{e.home, filepath.Join(e.dir, "omp-calls")} {
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
		"OMP_STUB_LOGIN="+login,
		"STUB_OMP_CALLS="+filepath.Join(e.dir, "omp-calls"),
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

// launches counts the interactive omp sessions the script started.
func (e *configureEnv) launches(t *testing.T) int {
	t.Helper()
	return strings.Count(readFile(t, filepath.Join(e.dir, "omp-calls")), "--allow-home\n")
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

// renderCredentials renders the returned credentials file the way the sandbox
// agent renders a templated harness file (sandbox-agent/terminal), and answers
// its entries by provider and email.
func (o configureOutput) renderCredentials(t *testing.T, sentinels map[string]string) (string, map[string]map[string]any) {
	t.Helper()
	file, ok := o.file(credentialsPath)
	if !ok || file["template"] != true {
		t.Fatalf("no templated credentials file in %v", o.Files)
	}
	tmpl, err := template.New("credentials").Option("missingkey=zero").Parse(file["content"].(string))
	if err != nil {
		t.Fatalf("credentials template does not parse: %v", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, map[string]any{"secrets": sentinels}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version     int `json:"version"`
		Credentials []struct {
			Provider   string         `json:"provider"`
			Credential map[string]any `json:"credential"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rendered.Bytes(), &doc); err != nil {
		t.Fatalf("rendered credentials are not JSON: %v\n%s", err, rendered.String())
	}
	if doc.Version != 1 {
		t.Fatalf("rendered credentials are version %d, want omp's export version 1", doc.Version)
	}
	entries := map[string]map[string]any{}
	for _, c := range doc.Credentials {
		key := c.Provider
		if email, _ := c.Credential["email"].(string); email != "" {
			key += " " + email
		}
		entries[key] = c.Credential
	}
	return rendered.String(), entries
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

//nolint:gosec // G101: sentinels a test renders, not credentials.
var signInSentinels = map[string]string{
	"OMP_OPENROUTER_CREDENTIAL":        "S-OPENROUTER",
	"OMP_ANTHROPIC_CREDENTIAL":         "S-ANTHROPIC",
	"OMP_ANTHROPIC_CREDENTIAL_2":       "S-ANTHROPIC-2",
	"OMP_OPENAI_CODEX_CREDENTIAL":      "S-CODEX",
	"OMP_GITHUB_COPILOT_CREDENTIAL":    "S-GH",
	"OMP_GOOGLE_GEMINI_CLI_CREDENTIAL": "S-GEMINI",
}

func prevEnv(sentinels map[string]string) map[string]string {
	out := map[string]string{}
	for name, value := range sentinels {
		out["PREV_"+name] = value
	}
	return out
}

// TestConfigureCapturesEverySignIn runs configure.sh against a stubbed omp
// that signs in the way /login leaves its store, and checks what the harness
// config would store and deliver — then reconfigures it, changing nothing.
func TestConfigureCapturesEverySignIn(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)

	if len(out.Secrets) != len(signInSentinels) {
		t.Fatalf("secrets = %v, want one per credential", out.Secrets)
	}
	value := func(name string) map[string]any {
		v, _ := out.secret(t, name)["value"].(map[string]any)
		return v
	}
	if s, v := out.secret(t, "OMP_OPENROUTER_CREDENTIAL"), value("OMP_OPENROUTER_CREDENTIAL"); s["type"] != "token" || v["token"] != "real-openrouter-key" {
		t.Fatalf("openrouter secret = %v, want a token", s)
	}
	// Anthropic's and ChatGPT's logins are renewed by the control plane, with
	// the clients the claude-code and codex images are renewed with.
	if s, v := out.secret(t, "OMP_ANTHROPIC_CREDENTIAL"), value("OMP_ANTHROPIC_CREDENTIAL"); s["type"] != "oauth" ||
		v["token"] != "real-anthropic-access" || v["refreshToken"] != "real-anthropic-refresh" ||
		v["tokenUrl"] != "https://console.anthropic.com/v1/oauth/token" || v["clientId"] != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Fatalf("anthropic secret = %v", s)
	}
	if s, v := out.secret(t, "OMP_ANTHROPIC_CREDENTIAL_2"), value("OMP_ANTHROPIC_CREDENTIAL_2"); s["type"] != "oauth" || v["token"] != "real-anthropic-access-2" {
		t.Fatalf("second anthropic account = %v, want a secret of its own", s)
	}
	if s, v := out.secret(t, "OMP_OPENAI_CODEX_CREDENTIAL"), value("OMP_OPENAI_CODEX_CREDENTIAL"); s["type"] != "oauth" ||
		v["tokenUrl"] != "https://auth.openai.com/oauth/token" || v["clientId"] != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("openai-codex secret = %v", s)
	}
	// GitHub Copilot's token does not expire and has nothing to refresh with,
	// and Gemini CLI's cannot be renewed from here.
	for _, name := range []string{"OMP_GITHUB_COPILOT_CREDENTIAL", "OMP_GOOGLE_GEMINI_CLI_CREDENTIAL"} {
		if s := out.secret(t, name); s["type"] != "token" {
			t.Fatalf("%s = %v, want a token", name, s)
		}
	}
	if !strings.Contains(out.Printed, "google-gemini-cli's sign-in expires 2030-03-17") {
		t.Errorf("configure did not say when the Gemini CLI login expires:\n%s", out.Printed)
	}
	if strings.Contains(out.Printed, "github-copilot's sign-in expires") {
		t.Errorf("configure warned about a token that does not expire:\n%s", out.Printed)
	}

	// Rendered the way a sandbox renders it, the file carries sentinels and
	// nothing real.
	rendered, entries := out.renderCredentials(t, signInSentinels)
	if strings.Contains(rendered, "real-") {
		t.Fatalf("delivered credentials hold a real credential: %s", rendered)
	}
	if e := entries["openrouter"]; e["type"] != "api_key" || e["key"] != "S-OPENROUTER" {
		t.Fatalf("delivered openrouter = %v", e)
	}
	if e := entries["anthropic me@example.com"]; e["access"] != "S-ANTHROPIC" || e["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		e["expires"] != float64(4102444800000) || e["accountId"] != "acct-1" {
		t.Fatalf("delivered anthropic = %v", e)
	}
	if e := entries["github-copilot"]; e["access"] != "S-GH" || e["refresh"] != "S-GH" {
		t.Fatalf("delivered copilot = %v, want the sentinel in both token fields", e)
	}
	// A login nothing renews keeps its real expiry: omp may as well know.
	if e := entries["google-gemini-cli"]; e["access"] != "S-GEMINI" || e["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		e["expires"] != float64(1900000000000) || e["projectId"] != "p-1" {
		t.Fatalf("delivered gemini = %v", e)
	}
	if file, ok := out.file(".omp/agent/config.yml"); !ok || !strings.Contains(file["content"].(string), "default: anthropic/claude-sonnet-5-5") {
		t.Fatalf("configure did not return omp's settings: %v", out.Files)
	}
	// The check is of omp's default model — what a sandbox starts on — so it
	// names none, and it uses no tools.
	calls := readFile(t, filepath.Join(env.dir, "omp-calls"))
	if !strings.Contains(calls, "\n-p --no-tools --no-session --no-title -- ") {
		t.Fatalf("configure did not check the default model tool-free: %s", calls)
	}

	// Reconfigure: the same harness, a user who changes nothing. Every
	// credential is imported as its sentinel and comes back as usePrevious.
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
	before, _ := out.file(credentialsPath)
	after, _ := again.file(credentialsPath)
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}
	if !strings.Contains(again.Printed, "Already signed in: openrouter, anthropic, openai-codex, github-copilot, google-gemini-cli.") {
		t.Errorf("reconfigure did not say what it opened signed in to:\n%s", again.Printed)
	}
}

// A sign-in made inside a kept session replaces that credential, and only it.
func TestConfigureReplacesOneCredentialInAKeptSession(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", `{"version":1,"credentials":[{"provider":"openrouter","credential":{"type":"api_key","key":"real-new-key"}}]}`,
		previous, prevEnv(signInSentinels))
	for _, secret := range again.Secrets {
		if secret["envName"] == "OMP_OPENROUTER_CREDENTIAL" {
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
// used, so it is neither offered nor kept.
func TestConfigureDropsACredentialWithNothingBehindIt(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", "", previous, map[string]string{"PREV_OMP_OPENROUTER_CREDENTIAL": "S-OPENROUTER"})
	if len(again.Secrets) != 1 || again.Secrets[0]["envName"] != "OMP_OPENROUTER_CREDENTIAL" || again.Secrets[0]["usePrevious"] != true {
		t.Fatalf("secrets = %v, want only the one with a sentinel, kept", again.Secrets)
	}
}

// A default model that does not answer is reported, and declining to start
// omp again saves every sign-in.
func TestConfigureKeepsSignInsWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\nn\n", signIns, nil, map[string]string{"OMP_STUB_RUN_FAILS": "1"})
	if len(out.Secrets) != len(signInSentinels) {
		t.Fatalf("secrets = %v, want every sign-in kept", out.Secrets)
	}
	if !strings.Contains(out.Printed, "omp's default model did not answer.") || !strings.Contains(out.Printed, "this model needs an opt-in") {
		t.Errorf("the failure was not reported with omp's reason:\n%s", out.Printed)
	}
}

// Starting omp again is a person's choice, and the round's sign-ins carry into
// the next one.
func TestConfigureRetriesOnlyWhenAsked(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\ny\n\nn\n", signIns, nil, map[string]string{"OMP_STUB_RUN_FAILS": "1"})
	if got := env.launches(t); got != 2 {
		t.Fatalf("omp started %d times, want 2", got)
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
	if _, ok := out.file(credentialsPath); ok {
		t.Fatal("configure returned a credentials file with nothing in it")
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

// Signing in again to the first of two accounts of one provider replaces that
// account's secret and keeps the other's, whatever order omp now exports them
// in: the stub, like omp, moves a replaced login to the end of its store. No
// secret is captured with a sentinel for its value.
func TestConfigureReplacesOneOfTwoAccountsByWhichItIs(t *testing.T) {
	env := newConfigureEnv(t)
	out := env.run(t, "\n", signIns, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", `{"version":1,"credentials":[{"provider":"anthropic","credential":{"type":"oauth","access":"real-anthropic-access-new","refresh":"real-anthropic-refresh-new","expires":1900000000000,"accountId":"acct-1","email":"me@example.com"}}]}`,
		previous, prevEnv(signInSentinels))
	first, second := again.secret(t, "OMP_ANTHROPIC_CREDENTIAL"), again.secret(t, "OMP_ANTHROPIC_CREDENTIAL_2")
	if v, _ := first["value"].(map[string]any); v["token"] != "real-anthropic-access-new" || first["name"] != "omp: anthropic (me@example.com)" {
		t.Fatalf("re-signed-in account = %v, want me@ with its new token", first)
	}
	if second["usePrevious"] != true || second["value"] != nil {
		t.Fatalf("other account = %v, want it kept", second)
	}
	for _, secret := range again.Secrets {
		if v, _ := secret["value"].(map[string]any); v != nil && strings.HasPrefix(v["token"].(string), "S-") {
			t.Fatalf("secret %v was captured with a sentinel for its value", secret)
		}
	}
	_, entries := again.renderCredentials(t, signInSentinels)
	if e := entries["anthropic work@example.com"]; e["access"] != "S-ANTHROPIC-2" {
		t.Fatalf("delivered work@ = %v, want its own sentinel", e)
	}
	if e := entries["anthropic me@example.com"]; e["access"] != "S-ANTHROPIC" {
		t.Fatalf("delivered me@ = %v, want its own sentinel", e)
	}
}

// A login nothing renews is reported when it is kept as well as when it is
// new, and one that has already expired says so.
func TestConfigureSaysWhenAKeptLoginExpires(t *testing.T) {
	env := newConfigureEnv(t)
	login := `{"version":1,"credentials":[
  {"provider":"google-gemini-cli","credential":{"type":"oauth","access":"real-gemini-access","refresh":"real-gemini-refresh","expires":1900000000000}},
  {"provider":"google-antigravity","credential":{"type":"oauth","access":"real-ag-access","refresh":"real-ag-refresh","expires":1600000000000}}
]}`
	out := env.run(t, "\n", login, nil, nil)
	previous := map[string]any{"files": out.Files, "secrets": stripValues(out.Secrets)}
	again := env.run(t, "\n", "", previous, map[string]string{
		"PREV_OMP_GOOGLE_GEMINI_CLI_CREDENTIAL":  "S-GEMINI",
		"PREV_OMP_GOOGLE_ANTIGRAVITY_CREDENTIAL": "S-AG",
	})
	for _, secret := range again.Secrets {
		if secret["usePrevious"] != true {
			t.Fatalf("secret %v, want it kept", secret)
		}
	}
	for _, want := range []string{"google-gemini-cli's sign-in expires 2030-03-17", "google-antigravity's sign-in expired 2020-09-13"} {
		if !strings.Contains(again.Printed, want) {
			t.Errorf("reconfigure did not say %q:\n%s", want, again.Printed)
		}
	}
}
