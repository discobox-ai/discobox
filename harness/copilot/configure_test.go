package copilot

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The stubbed copilot stores a login the way Copilot 1.0.91 does with no
// keychain: in config.json, headed by comment lines, under the account's key
// and again with the provider appended, beside lastLoggedInUser.
const copilotStub = `#!/bin/sh
state="${COPILOT_HOME:-$HOME/.copilot}/config.json"
store() {
	mkdir -p "$(dirname "$state")"
	host="${COPILOT_STUB_HOST:-https://github.com}"
	printf '// User settings belong in settings.json.\n// This file is managed automatically.\n{\n  "authTokens": {\n    "%s:octo:github": {"token": "%s"},\n    "%s:octo": {"token": "%s"}\n  },\n  "lastLoggedInUser": {"host": "%s", "login": "octo"}\n}\n' \
		"$host" "$1" "$host" "$1" "$host" >"$state"
}
case "$1" in
login)
	# copilot login --with-token reads the token from stdin.
	IFS= read -r token || exit 1
	case "$token" in
	ghp_*) echo "Classic personal access tokens are not supported." >&2; exit 1 ;;
	esac
	grep -q '"storeTokenPlaintext": true' "${COPILOT_HOME:-$HOME/.copilot}/settings.json" || {
		echo "Login succeeded, but the token was not saved." >&2; exit 1; }
	store "$token" ;;
--allow-all)
	# The interactive session: record the token it was handed, and do what the
	# user does in it.
	printf '%s' "${COPILOT_GITHUB_TOKEN:-}" >"$HOME/../session-token"
	if [ -n "${COPILOT_STUB_LOGIN:-}" ]; then store "$COPILOT_STUB_LOGIN"; fi
	exit "${COPILOT_STUB_SESSION_STATUS:-0}" ;;
*)
	# The verification prompt answers only for a token that works.
	if [ -z "${COPILOT_GITHUB_TOKEN:-}" ] || [ "$COPILOT_GITHUB_TOKEN" = "${COPILOT_STUB_REVOKED:-}" ]; then
		echo "Error: unauthorized" >&2; exit 1
	fi
	echo discobox-ok ;;
esac
`

type configureEnv struct {
	dir, home, bin, runDir, script string
}

func newConfigureEnv(t *testing.T) *configureEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "node", "timeout", "tail", "mktemp", "grep"} {
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
	for _, d := range []string{env.bin, env.runDir, filepath.Join(env.home, ".copilot")} {
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
	writeExecutable(t, filepath.Join(env.bin, "copilot"), copilotStub)
	// What a configured harness delivers: the settings the last run captured.
	if err := os.WriteFile(filepath.Join(env.home, ".copilot", "settings.json"), []byte("{\n  \"model\": \"claude-sonnet-5.5\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // A stub the script under test executes.
		t.Fatal(err)
	}
}

type configureOutput struct {
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
	Secrets []struct {
		EnvName     string `json:"envName"`
		Name        string `json:"name"`
		Type        string `json:"type"`
		UsePrevious bool   `json:"usePrevious"`
		Value       *struct {
			Token string `json:"token"`
		} `json:"value"`
	} `json:"secrets"`
}

// run runs configure.sh with input on stdin — one line per question it asks —
// and extra in the environment, and returns its output.
func (e *configureEnv) run(t *testing.T, input string, extra ...string) (configureOutput, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", e.script) //nolint:gosec // The configure script under test, copied into this test's directory.
	cmd.Dir = e.home
	cmd.Env = append(os.Environ(),
		"PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+e.home,
		"NO_COLOR=1",
	)
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("configure.sh output:\n%s", out)
		return configureOutput{}, err
	}
	raw, readErr := os.ReadFile(filepath.Join(e.runDir, "harness-configure.json"))
	if readErr != nil {
		t.Fatalf("configure.sh succeeded without writing its output: %v\n%s", readErr, out)
	}
	var result configureOutput
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result, nil
}

func (e *configureEnv) seedPrevious(t *testing.T, name string) {
	t.Helper()
	previous := `{"files":[],"secrets":[{"envName":"COPILOT_GITHUB_TOKEN","name":"` + name + `","type":"token"}]}`
	if err := os.WriteFile(filepath.Join(e.runDir, "harness-previous-config.json"), []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *configureEnv) sessionToken(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, "session-token"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A fine-grained PAT, handed to Copilot's own login, comes back as the one
// token the harness exports — and the settings come back without the
// plaintext-storage switch the script set to capture it.
func TestConfigureCapturesAFineGrainedToken(t *testing.T) {
	env := newConfigureEnv(t)
	got, err := env.run(t, "1\ngithub_pat_abc123\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 {
		t.Fatalf("secrets = %#v, want one", got.Secrets)
	}
	secret := got.Secrets[0]
	if secret.EnvName != "COPILOT_GITHUB_TOKEN" || secret.Type != "token" || secret.Value == nil || secret.Value.Token != "github_pat_abc123" {
		t.Fatalf("secret = %#v, want the PAT as a COPILOT_GITHUB_TOKEN token", secret)
	}
	if !strings.Contains(secret.Name, "fine-grained") {
		t.Errorf("secret name = %q, want it to say it is a fine-grained token", secret.Name)
	}
	if len(got.Files) != 1 || got.Files[0].Path != ".copilot/settings.json" {
		t.Fatalf("files = %#v, want the settings alone", got.Files)
	}
	content := got.Files[0].Content
	if !strings.Contains(content, "claude-sonnet-5.5") {
		t.Errorf("settings = %s, want the user's model kept", content)
	}
	if strings.Contains(content, "storeTokenPlaintext") {
		t.Errorf("settings = %s, want the script's own plaintext switch stripped", content)
	}
	// The session ran on the stored login, with no token in its environment to
	// shadow it.
	if token := env.sessionToken(t); token != "" {
		t.Errorf("session ran with COPILOT_GITHUB_TOKEN %q, want none", token)
	}
}

// /login has to clear a warning: a no goes back to the choice, and only a yes
// signs in with it. Its token is captured like any other.
func TestConfigureWarnsBeforeLogin(t *testing.T) {
	env := newConfigureEnv(t)
	got, err := env.run(t, "2\nn\n2\ny\n\n", "COPILOT_STUB_LOGIN=gho_fromlogin")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "gho_fromlogin" {
		t.Fatalf("secrets = %#v, want the /login token", got.Secrets)
	}
	if !strings.Contains(got.Secrets[0].Name, "OAuth") {
		t.Errorf("secret name = %q, want it to say it is an OAuth token", got.Secrets[0].Name)
	}
}

// The PAT prompt takes any token Copilot supports, and an OAuth token pasted
// there — gh's, say — carries the repository access a /login does, so it has
// to clear the same warning. Declining clears it and starts over.
func TestConfigureWarnsAboutABroadTokenPastedAsAPAT(t *testing.T) {
	env := newConfigureEnv(t)
	got, err := env.run(t, "1\ngho_fromgh\n\ny\n1\ngithub_pat_abc123\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "github_pat_abc123" {
		t.Fatalf("secrets = %#v, want the PAT entered after declining the OAuth token", got.Secrets)
	}

	accepted, err := newConfigureEnv(t).run(t, "1\ngho_fromgh\ny\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted.Secrets) != 1 || accepted.Secrets[0].Value == nil || accepted.Secrets[0].Value.Token != "gho_fromgh" ||
		!strings.Contains(accepted.Secrets[0].Name, "OAuth") {
		t.Fatalf("secrets = %#v, want the OAuth token, named as one, once the warning was accepted", accepted.Secrets)
	}
}

// The warning defaults to no: an empty answer goes back to the choice, so a
// user who signs in with a PAT next never has /login's token captured, even
// though the session would have stored one.
func TestConfigureLoginWarningDefaultsToNo(t *testing.T) {
	env := newConfigureEnv(t)
	got, err := env.run(t, "2\n\n1\ngithub_pat_abc123\n\n", "COPILOT_STUB_LOGIN=")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "github_pat_abc123" {
		t.Fatalf("secrets = %#v, want the PAT chosen after declining /login", got.Secrets)
	}
	// And end of input at the warning is not a yes.
	if _, err := newConfigureEnv(t).run(t, "2\n", "COPILOT_STUB_LOGIN=gho_fromlogin"); err == nil {
		t.Fatal("configure succeeded with nobody there to accept the /login warning")
	}
}

// A reconfigure opens signed in with the previous sentinel, and leaving it
// alone keeps it: usePrevious, no value.
func TestConfigureKeepsThePreviousToken(t *testing.T) {
	env := newConfigureEnv(t)
	env.seedPrevious(t, "GitHub fine-grained personal access token")
	got, err := env.run(t, "\n\n", "PREV_COPILOT_GITHUB_TOKEN=github_pat_sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || !got.Secrets[0].UsePrevious || got.Secrets[0].Value != nil {
		t.Fatalf("secrets = %#v, want the previous token kept by reference", got.Secrets)
	}
	if token := env.sessionToken(t); token != "github_pat_sentinel" { //nolint:gosec // A stand-in sentinel, not a credential.
		t.Errorf("session ran with COPILOT_GITHUB_TOKEN %q, want the previous sentinel", token)
	}
}

// Signing in to another account inside a kept session replaces the kept one:
// Copilot stores a /login even with a token in its environment. That token
// has the account's repository access, so it clears the warning first.
func TestConfigureReplacesTheKeptTokenWithANewLogin(t *testing.T) {
	env := newConfigureEnv(t)
	env.seedPrevious(t, "GitHub fine-grained personal access token")
	got, err := env.run(t, "\n\ny\n", "PREV_COPILOT_GITHUB_TOKEN=github_pat_sentinel", "COPILOT_STUB_LOGIN=gho_another")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].UsePrevious || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "gho_another" {
		t.Fatalf("secrets = %#v, want the new login", got.Secrets)
	}
}

// A previous token that no longer works stops being offered, and the retry
// signs in afresh.
func TestConfigureDropsARevokedPreviousToken(t *testing.T) {
	env := newConfigureEnv(t)
	env.seedPrevious(t, "GitHub fine-grained personal access token")
	got, err := env.run(t, "\n\ny\n1\ngithub_pat_fresh\n\n",
		"PREV_COPILOT_GITHUB_TOKEN=github_pat_sentinel", "COPILOT_STUB_REVOKED=github_pat_sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "github_pat_fresh" {
		t.Fatalf("secrets = %#v, want the fresh token", got.Secrets)
	}
}

// Only github.com can be delivered (ADR 26-10-02-840 §3), so a login anywhere
// else is refused, and declining the retry fails the flow.
func TestConfigureRefusesAnotherGitHubHost(t *testing.T) {
	env := newConfigureEnv(t)
	if _, err := env.run(t, "1\ngithub_pat_abc123\n\nn\n", "COPILOT_STUB_HOST=https://example.ghe.com"); err == nil {
		t.Fatal("configure accepted a login to a host the harness cannot deliver a token for")
	}
}

// A token Copilot refuses is retried only when a person asks.
func TestConfigureRetriesARefusedTokenOnlyWhenAsked(t *testing.T) {
	env := newConfigureEnv(t)
	got, err := env.run(t, "1\nghp_classic\ny\n1\ngithub_pat_abc123\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "github_pat_abc123" {
		t.Fatalf("secrets = %#v, want the token that worked", got.Secrets)
	}
	if _, err := newConfigureEnv(t).run(t, "1\nghp_classic\nn\n"); err == nil {
		t.Fatal("configure succeeded after the user declined to retry")
	}
}

// Choosing the PAT path decides nothing about what the session stores: a
// /login run inside it stores an OAuth token with repository access, and that
// is what would be saved, so it has to clear the same warning. Declining
// saves nothing.
func TestConfigureWarnsAboutALoginInsideTheSession(t *testing.T) {
	declined := newConfigureEnv(t)
	if _, err := declined.run(t, "1\ngithub_pat_abc123\n\nn\n", "COPILOT_STUB_LOGIN=gho_insession"); err == nil {
		t.Fatal("configure saved a /login token from inside the session without the warning being accepted")
	}
	if _, err := os.Stat(filepath.Join(declined.runDir, "harness-configure.json")); !os.IsNotExist(err) {
		t.Fatalf("configure wrote its output after the warning was declined: %v", err)
	}

	got, err := newConfigureEnv(t).run(t, "1\ngithub_pat_abc123\n\ny\n", "COPILOT_STUB_LOGIN=gho_insession")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value == nil || got.Secrets[0].Value.Token != "gho_insession" {
		t.Fatalf("secrets = %#v, want the token the session stored, once the warning was accepted", got.Secrets)
	}
}
