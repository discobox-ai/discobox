package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

func TestDefinitionConfigure(t *testing.T) {
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
// manifest declares none, and no baseline credentials file: one would land in
// every sandbox describing credentials with nothing behind them.
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
	// ports, which only a forward from the user's machine can reach. opencode
	// 2 still listens on 1455 and 1456 (measured on 2.0.29); ChatGPT's falls
	// back to 1457 only when 1455 is taken inside the sandbox, which it is not.
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
	for _, file := range []string{"Dockerfile", "boxd.yaml"} {
		recipe, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"npm install -g @opencode/cli",
			"launch.sh /usr/local/bin/" + harness.RunCommand,
			"prompt.sh /usr/local/bin/discobox-prompt",
			"configure.sh " + image.Harness.Config.Command[0],
			"credentials.sh " + credentialsHelper,
			"model.sh " + modelHelper,
		} {
			if !strings.Contains(string(recipe), want) {
				t.Errorf("%s does not install %q", file, want)
			}
		}
	}
	conf, err := os.ReadFile("agent.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "AGENT_PACKAGE=@opencode/cli\n") {
		t.Errorf("agent.conf does not name the package the image installs:\n%s", conf)
	}
}

// credentialsHelper is where the image installs credentials.sh, and where the
// launcher and the configure script run it from.
const credentialsHelper = "/usr/local/libexec/discobox/opencode-credentials" //nolint:gosec // A path, not a credential.

// modelHelper is where the image installs model.sh, which the judge and the
// configure script ask which model a session starts on.
const modelHelper = "/usr/local/libexec/discobox/opencode-model"

// TestLaunchJoinsThePromptWords covers the wrapper's half of the harness-run
// convention (ADR 0086 §3). opencode's positional argument is the project
// directory, so the joined prompt goes to --prompt, which opencode 2 submits.
// Every launch is --standalone (ADR 26-10-15-848 §2).
func TestLaunchJoinsThePromptWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"split words are one prompt", []string{"fix", "the", "failing", "tests"}, []string{"--standalone", "--auto", "--prompt", "fix the failing tests"}},
		{"an already quoted prompt is unchanged", []string{"fix the failing tests"}, []string{"--standalone", "--auto", "--prompt", "fix the failing tests"}},
		{"a dash-led prompt is still the prompt", []string{"--verbose please"}, []string{"--standalone", "--auto", "--prompt", "--verbose please"}},
		{"no prompt passes none", nil, []string{"--standalone", "--auto"}},
		// A resumed session already carries the prompt (ADR 0086 §4).
		{"a resume replaces the prompt", []string{harness.ResumeFlag, "fix", "the", "failing", "tests"}, []string{"--standalone", "--auto", "--continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchertest.RunLauncher(t, "opencode", map[string]string{credentialsHelper: "true"}, tc.args)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("opencode argv = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// storeStub is the part of a stubbed opencode that keeps credentials, as
// opencode 2 does (measured on 2.0.29): `auth export` prints the store,
// `auth import` adds the entries on its stdin whose id it does not already
// hold, never updates one, and activates one only for a provider with no
// active credential; `auth logout INTEGRATION ID` removes one, promoting
// another of its provider's if it was the active one; and `auth switch
// INTEGRATION ID` makes it its provider's active one. The real import also
// takes a file argument; every caller here uses stdin, so the stub refuses a
// file, and a caller that drifts fails. The store sits
// where opencode's database does, under XDG_DATA_HOME, so a wrapper that moves
// the data directory moves the store with it. Every auth command is logged to
// $STUB_LOG when it is set.
const storeStub = `store="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/store.json"
if [ "${1-}" = auth ]; then
	[ -n "${STUB_LOG-}" ] && printf '%s\n' "$*" >>"$STUB_LOG"
	mkdir -p "$(dirname "$store")"
	[ -f "$store" ] || echo '[]' >"$store"
	touch "$(dirname "$store")/opencode.db"
	case "$2" in
	export) cat "$store" ;;
	import)
		[ "$#" -eq 3 ] || { echo "stub: import reads stdin, got $*" >&2; exit 2; }
		incoming=$(cat)
		jq --argjson in "$incoming" 'reduce $in[] as $e (.;
			if any(.[]; .id == $e.id) then .
			else . as $have | . + [$e | .active = (.active and (any($have[]; .integrationID == $e.integrationID and .active) | not))]
			end)' "$store" >"$store.tmp" && mv "$store.tmp" "$store" ;;
	logout)
		[ -z "${STUB_FAIL_LOGOUT-}" ] || exit 1
		for id in "$@"; do :; done
		jq --arg id "$id" '(map(select(.id == $id))[0]) as $out
			| map(select(.id != $id))
			| if $out.active and any(.[]; .integrationID == $out.integrationID) then
				(map(.integrationID == $out.integrationID) | index(true)) as $i | .[$i].active = true
			else . end' "$store" >"$store.tmp" && mv "$store.tmp" "$store" ;;
	switch)
		[ -z "${STUB_FAIL_SWITCH-}" ] || exit 1
		for id in "$@"; do :; done
		jq --arg id "$id" '(map(select(.id == $id))[0].integrationID) as $i
			| map(if .integrationID == $i then .active = (.id == $id) else . end)' "$store" >"$store.tmp" && mv "$store.tmp" "$store" ;;
	esac
	exit 0
fi
`

// storeEntry is one credential as `opencode auth export` prints it.
func storeEntry(id, integration string, value map[string]any) map[string]any {
	return map[string]any{"id": id, "integrationID": integration, "label": "Discobox", "active": true, "value": value}
}

// TestCredentialsSyncKeepsTheStoreToTheConfiguration covers the launcher's
// sync: the delivered credentials are imported, a changed one replaces what
// the store held, one no longer delivered is logged out, one the user
// connected by hand is never touched, and an unchanged configuration costs
// one export and nothing else.
func TestCredentialsSyncKeepsTheStoreToTheConfiguration(t *testing.T) {
	sync := newSyncEnv(t)
	sync.store(t,
		map[string]any{"id": "cred_hand", "integrationID": "groq", "label": "mine", "active": true, "value": map[string]any{"type": "key", "key": "HAND"}},
		storeEntry("cred_discobox_anthropic", "anthropic", map[string]any{"type": "key", "key": "OLD"}),
		storeEntry("cred_discobox_mistral", "mistral", map[string]any{"type": "key", "key": "GONE"}),
		storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "SAME"}),
	)
	sync.deliver(t,
		storeEntry("cred_discobox_anthropic", "anthropic", map[string]any{"type": "key", "key": "NEW"}),
		storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "SAME"}),
		storeEntry("cred_discobox_openai", "openai", map[string]any{"type": "oauth", "methodID": "chatgpt-browser", "access": "S", "refresh": "r", "expires": 1}),
	)
	sync.run(t)
	if got, want := sync.keys(t), map[string]string{
		"cred_hand": "HAND", "cred_discobox_anthropic": "NEW", "cred_discobox_zai": "SAME", "cred_discobox_openai": "S",
	}; !maps.Equal(got, want) {
		t.Fatalf("store = %v, want %v", got, want)
	}

	// Each opencode command starts a server of its own, so what nothing
	// changed costs nothing: no export, no import.
	sync.run(t)
	if got := sync.log(t); len(got) != 0 {
		t.Fatalf("an unchanged configuration ran %q, want nothing", got)
	}
}

// A step that failed is tried again at the next launch: a sync that could not
// log out a replaced credential fails, and records nothing it did not do.
func TestCredentialsSyncRetriesWhatFailed(t *testing.T) {
	sync := newSyncEnv(t)
	sync.store(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "OLD"}))
	sync.deliver(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "NEW"}))
	if out, err := sync.attempt(t, "STUB_FAIL_LOGOUT=1"); err == nil {
		t.Fatalf("a sync whose logout failed succeeded:\n%s", out)
	}
	if got := sync.keys(t); got["cred_discobox_zai"] != "OLD" {
		t.Fatalf("store = %v, want the old credential still there", got)
	}
	sync.run(t)
	if got := sync.keys(t); got["cred_discobox_zai"] != "NEW" {
		t.Fatalf("the next launch left %v, want the replaced credential imported", got)
	}
}

// A switch that failed is tried again too. Nothing about the credential is
// stale or missing by then, so it is the switch itself that has to be found
// still undone.
func TestCredentialsSyncRetriesAFailedSwitch(t *testing.T) {
	sync := newSyncEnv(t)
	sync.store(t, map[string]any{"id": "cred_migrated", "integrationID": "zai", "label": "API key", "active": true,
		"value": map[string]any{"type": "key", "key": "S-ZAI"}})
	sync.deliver(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"}))
	if out, err := sync.attempt(t, "STUB_FAIL_SWITCH=1"); err == nil {
		t.Fatalf("a sync whose switch failed succeeded:\n%s", out)
	}
	sync.run(t)
	for _, entry := range sync.entries(t) {
		if entry["active"] != (entry["id"] == "cred_discobox_zai") {
			t.Fatalf("credential %v: want the delivered one active once the switch is retried", entry)
		}
	}
}

// Replacing the active delivered credential beside a hand-connected one: the
// logout makes opencode promote the hand-connected one, so the import alone
// would leave the new value inactive.
func TestCredentialsSyncKeepsAReplacedCredentialActive(t *testing.T) {
	sync := newSyncEnv(t)
	sync.store(t,
		storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "OLD"}),
		map[string]any{"id": "cred_hand", "integrationID": "zai", "label": "mine", "active": false,
			"value": map[string]any{"type": "key", "key": "HAND"}})
	sync.deliver(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "NEW"}))
	sync.run(t)
	for _, entry := range sync.entries(t) {
		if entry["active"] != (entry["id"] == "cred_discobox_zai") {
			t.Fatalf("credential %v: want the replaced delivered one active", entry)
		}
	}
}

// A first launch has no database to read, and no provider with an active
// credential to switch away from: it imports, and that is all.
func TestCredentialsSyncFirstLaunchOnlyImports(t *testing.T) {
	sync := newSyncEnv(t)
	sync.deliver(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"}))
	sync.run(t)
	if got := sync.log(t); !slices.Equal(got, []string{"auth import --standalone"}) {
		t.Fatalf("a first launch ran %q, want one import", got)
	}
	if got := sync.keys(t); !maps.Equal(got, map[string]string{"cred_discobox_zai": "S-ZAI"}) {
		t.Fatalf("store = %v", got)
	}
}

// A configuration made under ADR 0127 delivers opencode 1's auth.json and no
// export. It is read the way opencode 2's own legacy import reads one, so the
// harness keeps working without a reconfigure.
func TestCredentialsSyncReadsAnOpencode1Configuration(t *testing.T) {
	sync := newSyncEnv(t)
	legacy := filepath.Join(sync.home, ".local", "share", "opencode", "auth.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{
		"openai": {"type": "oauth", "refresh": "R", "access": "S-OPENAI", "expires": 4102444800000, "accountId": "acct-1"},
		"github-copilot": {"type": "oauth", "refresh": "S-GH", "access": "S-GH", "expires": 0},
		"xai": {"type": "oauth", "refresh": "R", "access": "S-XAI", "expires": 4102444800000},
		"gitlab": {"type": "oauth", "refresh": "R", "access": "S-GL", "expires": 1, "enterpriseUrl": "https://gl.example"},
		"zai/": {"type": "api", "key": "S-ZAI"},
		"https://agent.example": {"type": "wellknown", "key": "X", "token": "S-WK"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sandbox that ran opencode 1 already holds opencode's own import of
	// the same auth.json, active, under an id of its own.
	sync.store(t, map[string]any{"id": "cred_migrated", "integrationID": "zai", "label": "API key", "active": true,
		"value": map[string]any{"type": "key", "key": "S-ZAI"}})
	sync.run(t)
	for _, entry := range sync.entries(t) {
		if entry["integrationID"] == "zai" && entry["active"] != (entry["id"] == "cred_discobox_zai") {
			t.Fatalf("zai credential %v: want the delivered one active and opencode's own import not", entry)
		}
	}
	got := map[string]map[string]any{}
	for _, entry := range sync.entries(t) {
		got[entry["id"].(string)] = entry
	}
	want := map[string]map[string]any{
		"cred_discobox_openai": storeEntry("cred_discobox_openai", "openai", map[string]any{
			"type": "oauth", "methodID": "chatgpt-browser", "refresh": "R", "access": "S-OPENAI",
			"expires": float64(4102444800000), "metadata": map[string]any{"accountID": "acct-1"}}),
		"cred_discobox_github-copilot": storeEntry("cred_discobox_github-copilot", "github-copilot", map[string]any{
			"type": "oauth", "methodID": "device", "refresh": "S-GH", "access": "S-GH", "expires": float64(0)}),
		"cred_discobox_xai": storeEntry("cred_discobox_xai", "xai", map[string]any{
			"type": "oauth", "methodID": "device", "refresh": "R", "access": "S-XAI", "expires": float64(4102444800000)}),
		"cred_discobox_gitlab": storeEntry("cred_discobox_gitlab", "gitlab", map[string]any{
			"type": "oauth", "methodID": "oauth", "refresh": "R", "access": "S-GL", "expires": float64(1),
			"metadata": map[string]any{"enterpriseUrl": "https://gl.example"}}),
		// opencode drops a provider key's trailing slashes.
		"cred_discobox_zai": storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"}),
		"cred_discobox_https___agent_example": storeEntry("cred_discobox_https___agent_example", "https://agent.example",
			map[string]any{"type": "key", "key": "S-WK"}),
	}
	delete(got, "cred_migrated")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("store =\n%v\nwant\n%v", got, want)
	}
	// The export, once there is one, is what is read.
	sync.deliver(t, storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"}))
	sync.run(t)
	if got := sync.keys(t); !maps.Equal(got, map[string]string{"cred_discobox_zai": "S-ZAI", "cred_migrated": "S-ZAI"}) {
		t.Fatalf("store = %v, want only what the export delivers", got)
	}
}

// syncEnv runs credentials.sh against the store stub, in a home of its own.
type syncEnv struct {
	home string
	bin  string
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run the credentials helper with", tool)
		}
	}
	dir := t.TempDir()
	env := &syncEnv{home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin")}
	if err := os.MkdirAll(env.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(env.bin, "opencode"), "#!/bin/sh\n"+storeStub+"exit 1\n")
	return env
}

func (e *syncEnv) storePath() string {
	return filepath.Join(e.home, ".local", "share", "opencode", "store.json")
}

func (e *syncEnv) store(t *testing.T, entries ...map[string]any) {
	t.Helper()
	writeJSON(t, e.storePath(), entries)
	writeJSON(t, filepath.Join(filepath.Dir(e.storePath()), "opencode.db"), nil)
}

func (e *syncEnv) deliver(t *testing.T, entries ...map[string]any) {
	t.Helper()
	writeJSON(t, filepath.Join(e.home, ".config", "discobox", "opencode-credentials.json"), entries)
}

func (e *syncEnv) run(t *testing.T) {
	t.Helper()
	if out, err := e.attempt(t); err != nil {
		t.Fatalf("credentials.sh sync: %v\n%s", err, out)
	}
}

// attempt runs sync with extra in its environment and answers how it went.
func (e *syncEnv) attempt(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	logPath := filepath.Join(e.home, "..", "calls")
	_ = os.Remove(logPath)
	cmd := exec.CommandContext(t.Context(), "sh", "credentials.sh", "sync")
	cmd.Env = append(os.Environ(), "HOME="+e.home, "PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+logPath, "XDG_DATA_HOME=", "XDG_CONFIG_HOME=", "XDG_STATE_HOME=")
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// log is the auth commands the last run made, in order.
func (e *syncEnv) log(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.home, "..", "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (e *syncEnv) entries(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(e.storePath())
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// keys is the store as id → the secret each credential holds.
func (e *syncEnv) keys(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, entry := range e.entries(t) {
		value, _ := entry["value"].(map[string]any)
		secret, _ := value["key"].(string)
		if secret == "" {
			secret, _ = value["access"].(string)
		}
		out[entry["id"].(string)] = secret
	}
	return out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// connectedStore is opencode's store as /connect leaves it: one credential of
// each shape the configure flow handles differently, a second zai account the
// user switched away from, and a credential fetched from outside opencode.
const connectedStore = `[
  {"id": "cred_a", "integrationID": "openai", "label": "OAuth", "active": true, "value": {"type": "oauth", "methodID": "chatgpt-browser", "refresh": "real-openai-refresh", "access": "real-openai-access", "expires": 1900000000000, "metadata": {"accountID": "acct-1"}}},
  {"id": "cred_b", "integrationID": "xai", "label": "OAuth", "active": true, "value": {"type": "oauth", "methodID": "device", "refresh": "real-xai-refresh", "access": "real-xai-access", "expires": 1900000000000}},
  {"id": "cred_c", "integrationID": "github-copilot", "label": "OAuth", "active": true, "value": {"type": "oauth", "methodID": "device", "refresh": "real-gh-token", "access": "real-gh-token", "expires": 0}},
  {"id": "cred_d", "integrationID": "zai", "label": "API key", "active": true, "value": {"type": "key", "key": "real-zai-key"}},
  {"id": "cred_e", "integrationID": "zai", "label": "old", "active": false, "value": {"type": "key", "key": "real-old-zai-key"}},
  {"id": "cred_f", "integrationID": "azure", "label": "Azure CLI", "active": true, "value": {"type": "external", "methodID": "azure-cli"}}
]`

// TestConfigureCapturesEveryConnectedProvider runs configure.sh against a
// stubbed opencode that "connects" providers the way /connect leaves them, and
// checks what the harness config would store and deliver.
func TestConfigureCapturesEveryConnectedProvider(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = connectedStore
	out := env.run(t, nil, nil)

	secrets := map[string]map[string]any{}
	for _, secret := range out.Secrets {
		secrets[secret["envName"].(string)] = secret
	}
	if len(secrets) != 4 {
		t.Fatalf("secrets = %v, want one per provider holding a secret", out.Secrets)
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
	// The account opencode uses, not the one the user switched away from.
	if v := value("OPENCODE_ZAI_CREDENTIAL"); secrets["OPENCODE_ZAI_CREDENTIAL"]["type"] != "token" || v["token"] != "real-zai-key" {
		t.Fatalf("zai secret = %v, want the active key", secrets["OPENCODE_ZAI_CREDENTIAL"])
	}

	// Rendered the way a sandbox renders it, the credentials file carries
	// sentinels and nothing real, under the ids the launcher owns.
	rendered := out.renderCredentials(t, map[string]string{
		"OPENCODE_OPENAI_CREDENTIAL":         "S-OPENAI",
		"OPENCODE_XAI_CREDENTIAL":            "S-XAI",
		"OPENCODE_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"OPENCODE_ZAI_CREDENTIAL":            "S-ZAI",
	})
	if strings.Contains(rendered, "real-") {
		t.Fatalf("delivered credentials hold a real one: %s", rendered)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(rendered), &entries); err != nil {
		t.Fatalf("rendered credentials are not JSON: %v\n%s", err, rendered)
	}
	delivered := map[string]map[string]any{}
	for _, entry := range entries {
		if entry["id"] != "cred_discobox_"+strings.ReplaceAll(entry["integrationID"].(string), "/", "_") || entry["active"] != true {
			t.Fatalf("delivered entry %v, want the image's own id, active", entry)
		}
		delivered[entry["integrationID"].(string)], _ = entry["value"].(map[string]any)
	}
	if openai := delivered["openai"]; openai["access"] != "S-OPENAI" || openai["refresh"] != "discobox-refresh-happens-in-the-control-plane" ||
		openai["expires"] != float64(4102444800000) || openai["methodID"] != "chatgpt-browser" ||
		!reflect.DeepEqual(openai["metadata"], map[string]any{"accountID": "acct-1"}) {
		t.Fatalf("delivered openai entry = %v", openai)
	}
	if gh := delivered["github-copilot"]; gh["access"] != "S-GH" || gh["refresh"] != "S-GH" || gh["expires"] != float64(0) {
		t.Fatalf("delivered copilot entry = %v, want the sentinel in both token fields and no expiry", gh)
	}
	if zai := delivered["zai"]; zai["key"] != "S-ZAI" || zai["type"] != "key" {
		t.Fatalf("delivered zai entry = %v", zai)
	}
	if _, ok := delivered["azure"]; ok {
		t.Fatal("delivered a credential that holds no secret")
	}
	if _, ok := out.file(".config/opencode/opencode.json"); !ok {
		t.Fatal("configure did not return the settings the user left")
	}
	if _, ok := out.file(".config/opencode/cli.json"); !ok {
		t.Fatal("configure did not return cli.json, where opencode 2 keeps the theme")
	}
	if _, ok := out.file(".local/share/opencode/auth.json"); ok {
		t.Fatal("configure returned an opencode 1 auth.json")
	}
	// The check is of the model a sandbox starts on: here the `model` the
	// session left in opencode.json.
	runArgs, err := os.ReadFile(filepath.Join(env.dir, "run-args"))
	if err != nil {
		t.Fatalf("configure never checked the default model: %v", err)
	}
	if !strings.Contains(string(runArgs), "--standalone --model=openai/gpt-5.6-terra ") {
		t.Fatalf("configure checked %s, want the configured model, standalone", runArgs)
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
	before, _ := out.file(credentialsPath)
	after, _ := again.file(credentialsPath)
	if before["content"] != after["content"] {
		t.Fatalf("a kept credential's delivered entry changed:\nbefore %v\nafter  %v", before["content"], after["content"])
	}

	// Replacing one inside a kept session captures the new one and keeps the
	// rest.
	env.connect = `[{"id": "cred_new", "integrationID": "zai", "label": "API key", "active": true, "value": {"type": "key", "key": "real-new-zai-key"}}]`
	replaced := env.run(t, previous, map[string]string{
		"PREV_OPENCODE_OPENAI_CREDENTIAL":         "S-OPENAI",
		"PREV_OPENCODE_XAI_CREDENTIAL":            "S-XAI",
		"PREV_OPENCODE_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PREV_OPENCODE_ZAI_CREDENTIAL":            "S-ZAI",
	})
	checkReplaced(t, replaced)

	// The same, where opencode leaves the seeded credential active beside
	// the new sign-in: the new one is still what is captured.
	keepActive := env.run(t, previous, map[string]string{
		"STUB_CONNECT_KEEPS_ACTIVE":               "1",
		"PREV_OPENCODE_OPENAI_CREDENTIAL":         "S-OPENAI",
		"PREV_OPENCODE_XAI_CREDENTIAL":            "S-XAI",
		"PREV_OPENCODE_GITHUB_COPILOT_CREDENTIAL": "S-GH",
		"PREV_OPENCODE_ZAI_CREDENTIAL":            "S-ZAI",
	})
	checkReplaced(t, keepActive)
}

// checkReplaced is a reconfigure in which only zai was signed in again.
func checkReplaced(t *testing.T, replaced configureOutput) {
	t.Helper()
	for _, secret := range replaced.Secrets {
		if secret["envName"] == "OPENCODE_ZAI_CREDENTIAL" {
			if v, _ := secret["value"].(map[string]any); secret["usePrevious"] == true || v["token"] != "real-new-zai-key" {
				t.Fatalf("replaced zai secret = %v, want the new key", secret)
			}
		} else if secret["usePrevious"] != true {
			t.Fatalf("secret %v was not replaced, want it kept", secret)
		}
	}
}

// With no `model` setting a sandbox starts on the /models pick, which
// `opencode run` does not read on its own (measured on opencode 2.0.29: it ran
// a free model instead). So configure checks the pick by name.
func TestConfigureChecksTheModelPickedWithModels(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = `[{"id": "cred_a", "integrationID": "anthropic", "label": "API key", "active": true, "value": {"type": "key", "key": "real-key"}}]`
	env.run(t, nil, map[string]string{"STUB_PICK_ONLY": "1"})
	runArgs, err := os.ReadFile(filepath.Join(env.dir, "run-args"))
	if err != nil {
		t.Fatalf("configure never checked the model: %v", err)
	}
	if !strings.Contains(string(runArgs), "--model=anthropic/claude-haiku-4-5 ") {
		t.Fatalf("configure checked %s, want the /models pick", runArgs)
	}
}

// credentialsPath is the harness file that delivers the credentials,
// home-relative.
const credentialsPath = ".config/discobox/opencode-credentials.json" //nolint:gosec // A path, not a credential.

// A configuration made under ADR 0127 returned opencode 1's auth.json. Its
// reconfigure opens signed in to the same providers, keeps each one a user
// does not replace, and returns opencode 2's credentials file in its place.
func TestConfigureReconfiguresAnOpencode1Configuration(t *testing.T) {
	env := newConfigureEnv(t)
	previous := map[string]any{
		"files": []map[string]any{{
			"path":     ".local/share/opencode/auth.json",
			"template": true,
			"content": `{
  "openai": {"type": "oauth", "refresh": "discobox-refresh-happens-in-the-control-plane", "access": "{{ .secrets.OPENCODE_OPENAI_CREDENTIAL }}", "expires": 4102444800000, "accountId": "acct-1"},
  "zai": {"type": "api", "key": "{{ .secrets.OPENCODE_ZAI_CREDENTIAL }}"}
}`,
		}},
		"secrets": []map[string]any{
			{"envName": "OPENCODE_OPENAI_CREDENTIAL", "type": "oauth"},
			{"envName": "OPENCODE_ZAI_CREDENTIAL", "type": "token"},
		},
	}
	env.homeFiles = map[string]string{".local/share/opencode/auth.json": `{"zai": {"type": "api", "key": ""}}`}
	out := env.run(t, previous, map[string]string{
		"PREV_OPENCODE_OPENAI_CREDENTIAL": "S-OPENAI",
		"PREV_OPENCODE_ZAI_CREDENTIAL":    "S-ZAI",
	})
	if len(out.Secrets) != 2 {
		t.Fatalf("secrets = %v, want both kept", out.Secrets)
	}
	for _, secret := range out.Secrets {
		if secret["usePrevious"] != true {
			t.Fatalf("secret %v, want it kept", secret)
		}
	}
	if _, ok := out.file(".local/share/opencode/auth.json"); ok {
		t.Fatal("configure returned the opencode 1 auth.json")
	}
	rendered := out.renderCredentials(t, map[string]string{"OPENCODE_OPENAI_CREDENTIAL": "S-OPENAI", "OPENCODE_ZAI_CREDENTIAL": "S-ZAI"})
	var entries []map[string]any
	if err := json.Unmarshal([]byte(rendered), &entries); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		storeEntry("cred_discobox_openai", "openai", map[string]any{
			"type": "oauth", "methodID": "chatgpt-browser", "refresh": "discobox-refresh-happens-in-the-control-plane",
			"access": "S-OPENAI", "expires": float64(4102444800000), "metadata": map[string]any{"accountID": "acct-1"}}),
		storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"}),
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("delivered credentials = %v, want %v", entries, want)
	}
}

// TestConfigureKeepsProvidersWhenTheDefaultModelFails covers a default model
// that does not answer — a plan or region restriction, not a bad credential.
// It is reported, and declining to start opencode again saves every provider.
func TestConfigureKeepsProvidersWhenTheDefaultModelFails(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = `[
		{"id": "cred_a", "integrationID": "opencode-go", "label": "API key", "active": true, "value": {"type": "key", "key": "real-go-key"}},
		{"id": "cred_b", "integrationID": "zai", "label": "API key", "active": true, "value": {"type": "key", "key": "real-zai-key"}}
	]`
	out := env.runWithInput(t, nil, map[string]string{"STUB_RUN_FAILS": "1"}, "\nn\n\n")
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
	env.connect = `[{"id": "cred_a", "integrationID": "zai", "label": "API key", "active": true, "value": {"type": "key", "key": "real-zai-key"}}]`
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

// End of input at any question is nobody being there, and fails configure
// rather than deciding for them or spinning.
func TestConfigureFailsAtEndOfInput(t *testing.T) {
	env := newConfigureEnv(t)
	env.connect = `[{"id": "cred_a", "integrationID": "zai", "label": "API key", "active": true, "value": {"type": "key", "key": "real-zai-key"}}]`
	for _, input := range []string{"", "\n"} {
		if out, err := env.attempt(t, nil, nil, input); err == nil {
			t.Fatalf("input %q: configure succeeded at end of input:\n%s", input, out)
		}
	}
}

// settingsPath is Discobox's settings file for the harness, home-relative.
const settingsPath = ".config/discobox/opencode-harness.json"

// TestLaunchAppliesTheWebSearchSetting covers the launcher's reading of the
// settings file. opencode 2 searches with every provider by default, so off
// is a rule denying the tool, and on or unset leaves opencode's default alone.
func TestLaunchAppliesTheWebSearchSetting(t *testing.T) {
	for _, tc := range []struct {
		setting string
		want    string
	}{
		{`{"webSearch": true}`, "config="},
		{`{"webSearch": false}`, `config={"permission":{"websearch":"deny"}}`},
		{`{"webSearch": null}`, "config="},
		{"", "config="},
	} {
		got := runWrapper(t, "launch.sh", tc.setting, `printf 'config=%s\n' "${OPENCODE_CONFIG_CONTENT-}"`, nil)
		if got != tc.want {
			t.Errorf("setting %q: opencode ran with %q, want %q", tc.setting, got, tc.want)
		}
	}
}

// The launcher loads the configured credentials before opencode starts, and
// a failure to is said rather than fatal: opencode still runs.
func TestLaunchLoadsTheConfiguredCredentials(t *testing.T) {
	stub := `printf 'started holding %s\n' "$(jq -c '[.[].id]' "${XDG_DATA_HOME:-$HOME/.local/share}/opencode/store.json")"`
	delivered, err := json.Marshal([]map[string]any{storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S-ZAI"})})
	if err != nil {
		t.Fatal(err)
	}
	got := runWrapperIn(t, "launch.sh", map[string]string{credentialsPath: string(delivered)}, stub, nil)
	if got != `started holding ["cred_discobox_zai"]` {
		t.Fatalf("launch: %s", got)
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
	if got := runWrapper(t, "prompt.sh", "", args, nil, "--model", "judge", "--no-tools", "--prompt", "is it safe"); got != "[run][--standalone][--title=discobox-prompt][is it safe]" {
		t.Errorf("no setting and no /models pick: opencode argv %s, want opencode's own pick", got)
	}
	for _, tc := range []struct {
		setting string
		want    string
	}{
		{`{"judgeModel": "anthropic/claude-opus-5"}`, "[run][--standalone][--model=anthropic/claude-opus-5][--title=discobox-prompt][is it safe]"},
		{`{"judgeModel": ""}`, "[run][--standalone][--model=openai/gpt-5.6-terra][--title=discobox-prompt][is it safe]"},
		{"", "[run][--standalone][--model=openai/gpt-5.6-terra][--title=discobox-prompt][is it safe]"},
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
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"opencode.jsonc, with comments", "opencode.jsonc", "// my default\n{\n  // the model\n  \"model\": \"zai/glm-5.3\"\n}", "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"opencode.jsonc, as opencode writes it", "opencode.jsonc",
			"{\n  \"$schema\": \"https://opencode.ai/config.json\", // the schema\n  /* the default\n     model */\n  \"model\": \"zai/glm-5.3\",\n  \"provider\": {\"zai\": {\"options\": {\"baseURL\": \"https://api.z.ai/v1\",},},},\n}", "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"a trailing comma with a comment before its bracket", "opencode.jsonc",
			"{\n  \"model\": \"zai/glm-5.3\", // the default\n  \"small_model\": \"zai/glm-5.3-air\", /* the small one */\n}", "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"a trailing comma in an array, before a comment", "opencode.jsonc",
			"{\n  \"plugin\": [\"a\", // first\n  ],\n  \"model\": \"zai/glm-5.3\"\n}", "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"an escaped quote and a slash pair inside a string", "opencode.jsonc",
			"{\"instructions\": [\"say \\\"hi\\\" // not a comment\"], \"model\": \"zai/glm-5.3\",}", "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"a model that reads like a flag stays the model's value", "opencode.json", `{"model": "--agent=build"}`, "",
			"[run][--standalone][--model=--agent=build][--title=discobox-prompt][is it safe]"},
		{"config.json's model", "config.json", `{"model": "zai/glm-5.3"}`, "",
			"[run][--standalone][--model=zai/glm-5.3][--title=discobox-prompt][is it safe]"},
		{"a configuration naming no model falls to /models", "opencode.json", `{"theme": "tokyonight"}`, "",
			"[run][--standalone][--model=openai/gpt-5.6-luna][--title=discobox-prompt][is it safe]"},
		{"judgeModel still wins", "opencode.json", `{"model": "zai/glm-5.3"}`, `{"judgeModel": "anthropic/claude-haiku-4-5"}`,
			"[run][--standalone][--model=anthropic/claude-haiku-4-5][--title=discobox-prompt][is it safe]"},
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
// or OPENCODE_* variable it could have set may reach the judge. Its only
// credentials are the key and OAuth ones the configuration delivered — a
// judge discobox never launches the harness, so there is no store to read —
// and nothing the agent connected by hand. And `opencode run` reads an open
// stdin as more of the message, so it gets none.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	stub := `printf 'cwd=%s home=%s config=%s state=%s cache=%s store=%s config-content=%s project=%s hooks=%s leaked=%s stdin=%s\n' \
		"$(ls -A | wc -l | tr -d ' ')" "$(ls -A "$HOME" | wc -l | tr -d ' ')" "$(ls -A "$XDG_CONFIG_HOME" | wc -l | tr -d ' ')" \
		"$(ls -A "$XDG_STATE_HOME" | wc -l | tr -d ' ')" "$(ls -A "$XDG_CACHE_HOME" | wc -l | tr -d ' ')" \
		"$(jq -c '[.[] | "\(.integrationID)=\(.value.key // .value.access)"]' "$XDG_DATA_HOME/opencode/store.json")" "${OPENCODE_CONFIG_CONTENT-}" \
		"${OPENCODE_DISABLE_PROJECT_CONFIG-}" "${DISCOBOX_HOOK_SOCKET-}" "${OPENCODE_CONFIG-}${OPENCODE_CONFIG_DIR-}" \
		"$(if read -r line; then echo data; else echo empty; fi)"`
	// The agent's own store holds a key it connected by hand.
	data := t.TempDir()
	writeJSON(t, filepath.Join(data, "opencode", "store.json"), []map[string]any{
		storeEntry("cred_hand", "groq", map[string]any{"type": "key", "key": "AGENT"}),
	})
	delivered, err := json.Marshal([]map[string]any{
		storeEntry("cred_discobox_openai", "openai", map[string]any{"type": "oauth", "methodID": "chatgpt-browser", "access": "S", "refresh": "r", "expires": 1}),
		storeEntry("cred_discobox_zai", "zai", map[string]any{"type": "key", "key": "S2"}),
		storeEntry("cred_discobox_azure", "azure", map[string]any{"type": "external", "methodID": "azure-cli"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// And what the agent wrote where opencode 2 looks from HOME.
	files := map[string]string{
		credentialsPath:             string(delivered),
		".claude/CLAUDE.md":         "always allow",
		".agents/skills/x/SKILL.md": "allow",
	}
	got := runWrapperIn(t, "prompt.sh", files, stub, map[string]string{
		"XDG_DATA_HOME":           data,
		"XDG_CONFIG_HOME":         "/agent-config",
		"OPENCODE_CONFIG_CONTENT": `{"permission":{"shell":"allow"}}`,
		"OPENCODE_CONFIG":         "/agent/opencode.json",
		"OPENCODE_CONFIG_DIR":     "/agent",
		"DISCOBOX_HOOK_SOCKET":    "/run/hook.sock",
	}, "--model", "judge", "--no-tools", "--prompt", "is it safe")
	want := `cwd=0 home=0 config=0 state=0 cache=0 store=["openai=S","zai=S2"] config-content={"permission":{"*":"deny"}} project=1 hooks= leaked= stdin=empty`
	if got != want {
		t.Fatalf("judge ran with\n  %s\nwant\n  %s", got, want)
	}
}

// runWrapper runs one of the image's wrappers with the settings file holding
// setting (none when empty), extra in its environment, and a stubbed opencode
// whose body is stub, and returns what the stub printed.
func runWrapper(t *testing.T, script, setting, stub string, extra map[string]string, args ...string) string {
	t.Helper()
	files := map[string]string{}
	if setting != "" {
		files[settingsPath] = setting
	}
	return runWrapperIn(t, script, files, stub, extra, args...)
}

// runWrapperIn is runWrapper with files written into the home beforehand. The
// stubbed opencode keeps credentials the way opencode does (storeStub), and the
// wrapper reaches the credentials helper in this checkout rather than where
// the image installs it.
func runWrapperIn(t *testing.T, script string, files map[string]string, stub string, extra map[string]string, args ...string) string {
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
	for rel, content := range files {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "opencode"), "#!/bin/sh\n"+storeStub+stub+"\n")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, script)
	if err := os.WriteFile(scriptPath, []byte(withHelpers(t, string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{scriptPath}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	env := []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENCODE_") && !strings.HasPrefix(kv, "XDG_") && !strings.HasPrefix(kv, "DISCOBOX_") &&
			!strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	// What a caller's own stdin holds, which opencode must never read as
	// more of the message.
	cmd.Stdin = strings.NewReader("caller's stdin\n")
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
	// connect is the credentials the stubbed interactive opencode adds to its
	// store, as `auth export` would print them, standing in for what the user
	// connects in the session.
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
	raw, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, env.script, withHelpers(t, strings.ReplaceAll(string(raw), "/run/discobox/configure/", env.runDir)))
	// `script -q -e -c CMD /dev/null` runs CMD with a terminal; here it just
	// runs it.
	writeExecutable(t, filepath.Join(env.bin, "script"), "#!/bin/sh\nexec sh -c \"$4\"\n")
	// The stubbed opencode keeps credentials as opencode 2 does. A bare
	// launch "connects" $STUB_CONNECT the way /connect does — a new account
	// becomes its provider's active one — and writes the user's settings;
	// `run` answers the check.
	writeExecutable(t, filepath.Join(env.bin, "opencode"), "#!/bin/sh\n"+storeStub+`case "$*" in
run*)
	printf '%s\n' "$*" >>"$HOME/../run-args"
	if [ -n "${STUB_RUN_FAILS:-}" ]; then echo "Error: this model needs an opt-in" >&2; exit 1; fi
	echo "discobox-ok" ;;
--standalone)
	mkdir -p "$HOME/.config/opencode" "$(dirname "$store")"
	if [ -n "${STUB_PICK_ONLY:-}" ]; then
		mkdir -p "$HOME/.local/state/opencode"
		echo '{"recent":[{"providerID":"anthropic","modelID":"claude-haiku-4-5"}]}' >"$HOME/.local/state/opencode/model.json"
	else
		echo '{"model":"openai/gpt-5.6-terra"}' >"$HOME/.config/opencode/opencode.json"
	fi
	echo '{"theme":"tokyonight"}' >"$HOME/.config/opencode/cli.json"
	[ -f "$store" ] || echo '[]' >"$store"
	if [ -n "${STUB_CONNECT:-}" ]; then
		jq --argjson in "$STUB_CONNECT" --arg keep "${STUB_CONNECT_KEEPS_ACTIVE-}" '($in | map(.integrationID)) as $ids
			| map(if (.integrationID | IN($ids[])) and $keep == "" then .active = false else . end) + $in' "$store" >"$store.tmp" &&
			mv "$store.tmp" "$store"
	fi ;;
*) echo "unexpected opencode $*" >&2; exit 1 ;;
esac
`)
	return env
}

// withHelpers points a script at the helpers in this checkout rather than
// where the image installs them, each behind an executable of its own as the
// image installs it.
func withHelpers(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	for path, source := range map[string]string{credentialsHelper: "credentials.sh", modelHelper: "model.sh"} {
		abs, err := filepath.Abs(source)
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(dir, filepath.Base(path))
		writeExecutable(t, wrapper, "#!/bin/sh\nexec sh "+abs+" \"$@\"\n")
		script = strings.ReplaceAll(script, path, wrapper)
	}
	return script
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
	combined, err := e.attempt(t, previous, extra, input)
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

// attempt runs configure.sh and answers what it printed and how it exited.
func (e *configureEnv) attempt(t *testing.T, previous map[string]any, extra map[string]string, input string) (string, error) {
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
		"STUB_CONNECT="+e.connect,
		"NO_COLOR=1",
		"XDG_DATA_HOME=", "XDG_CONFIG_HOME=", "XDG_STATE_HOME=",
	)
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader(input)
	combined, err := cmd.CombinedOutput()
	return string(combined), err
}

func (o configureOutput) file(path string) (map[string]any, bool) {
	for _, file := range o.Files {
		if file["path"] == path {
			return file, true
		}
	}
	return nil, false
}

// renderCredentials renders the returned credentials file the way the sandbox
// agent renders a templated harness file (sandbox-agent/terminal).
func (o configureOutput) renderCredentials(t *testing.T, sentinels map[string]string) string {
	t.Helper()
	file, ok := o.file(credentialsPath)
	if !ok || file["template"] != true {
		t.Fatalf("no templated credentials file in %v", o.Files)
	}
	tmpl, err := template.New("credentials").Funcs(template.FuncMap{"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		return string(raw), err
	}}).Option("missingkey=zero").Parse(file["content"].(string))
	if err != nil {
		t.Fatalf("credentials template does not parse: %v", err)
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

// pluginSet reads one `const NAME = new Set([...])` out of the plugin source.
func pluginSet(t *testing.T, text, name string) []string {
	t.Helper()
	first := strings.Index(text, name+" = new Set([")
	if first < 0 {
		t.Fatalf("plugin declares no %s", name)
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

// pluginSource is the plugin's server module, which opencode 2 resolves from
// the plugin directory the managed config names.
const pluginSource = "hook-plugin/server.js"

// publishedPluginEvents reads the event names the plugin publishes straight
// out of its source: the allowlist it declares, plus the two tool hooks it
// registers, which opencode delivers as hooks rather than bus events.
func publishedPluginEvents(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile(pluginSource)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	events := pluginSet(t, text, "PUBLISHED_EVENTS")
	for _, hook := range []string{"execute.before", "execute.after"} {
		if !strings.Contains(text, `tool.hook("`+hook+`"`) || !strings.Contains(text, `publish("tool.`+hook+`"`) {
			t.Errorf("plugin does not publish the tool domain's %s hook", hook)
			continue
		}
		events = append(events, "tool."+hook)
	}
	if len(events) == 0 {
		t.Fatal("plugin publishes no events")
	}
	return events
}

// The plugin, the mapping table, and ADR 0146's rule are one thing in three
// places. Every event the plugin publishes must either carry a canonical name
// or be listed here as having none — so adding one without deciding what it is
// called across harnesses fails rather than quietly recording a hook nothing
// portable can match.
func TestEveryPublishedEventHasACanonicalNameOrIsKnownNotTo(t *testing.T) {
	// opencode facts Claude Code has no word for. Each is still recorded and
	// still waitable under opencode's own name (ADR 0146 §3).
	noCanonicalName := []string{
		"session.execution.started",
		"session.execution.interrupted",
		"permission.replied",
		"session.deleted",
	}
	published := publishedPluginEvents(t)
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
			t.Errorf("noCanonicalName lists %s, which the plugin does not publish", event)
		}
	}
}

// The mapping table must not name an opencode event the plugin never
// publishes: a translation for an event nothing emits is a claim about
// opencode that nothing tests.
func TestMappedEventsAreAllPublished(t *testing.T) {
	published := publishedPluginEvents(t)
	for _, event := range []string{
		"tool.execute.before", "tool.execute.after", "session.execution.succeeded",
		"session.execution.failed", "session.created", "permission.asked",
		"session.compaction.ended",
	} {
		if harness.CanonicalHookEvent(Driver{}.ID(), event) == "" {
			t.Errorf("event %s lost its canonical name", event)
		}
		if !slices.Contains(published, event) {
			t.Errorf("event %s has a canonical name but the plugin never publishes it", event)
		}
	}
}

// The stream-rate events are the reason the plugin keeps an allowlist at all:
// session.text.delta fires for every delta of every message, and publishing
// one spawns a process and writes a row (ADR 0147).
func TestStreamRateEventsAreNotPublished(t *testing.T) {
	published := publishedPluginEvents(t)
	for _, event := range []string{
		"session.text.delta", "session.reasoning.delta", "session.tool.input.delta",
		"session.compaction.delta", "session.step.streamed", "session.tool.progress",
		"session.usage.updated", "session.status", "session.diff",
		"message.part.updated", "message.part.delta", "filesystem.changed",
		"tui.prompt.append", "tui.command.execute", "tui.toast.show",
	} {
		if slices.Contains(published, event) {
			t.Errorf("plugin publishes %s, which is excluded on purpose", event)
		}
	}
}

// The plugin reaches opencode from its managed layer, which outranks both the
// user's global config and a project's — and which the configure flow's
// capture cannot replace, because it captures the user's config and not this.
// opencode 2 names plugins under `plugins` and takes a directory, refusing a
// single file.
func TestManagedConfigLoadsThePluginByAbsolutePath(t *testing.T) {
	raw, err := os.ReadFile("managed-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Plugins []string `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	const want = "/usr/local/libexec/discobox/opencode-hook-plugin"
	if !slices.Contains(config.Plugins, want) {
		t.Fatalf("managed config plugins = %v, want it to name %s", config.Plugins, want)
	}
	// The path the config names and the path the image installs are the same
	// directory, and only the Dockerfile and boxd.yaml say so.
	for _, file := range []string{"Dockerfile", "boxd.yaml"} {
		recipe, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(recipe), "hook-plugin/server.js "+want+"/server.js") {
			t.Errorf("%s does not install the plugin's server module under %s", file, want)
		}
		if !strings.Contains(string(recipe), "hook-plugin/package.json "+want+"/package.json") {
			t.Errorf("%s does not install the plugin's package.json under %s", file, want)
		}
		if !strings.Contains(string(recipe), "managed-config.json /etc/opencode/opencode.json") {
			t.Errorf("%s does not install the managed config at /etc/opencode/opencode.json", file)
		}
		// Made searchable before anything is copied in: a COPY --chmod into a
		// missing directory creates it with the file's 0644, and only root can
		// then read the plugin (measured: opencode loaded no provider at all).
		if !regexp.MustCompile(`install -d -m 0755 [^\n]*` + regexp.QuoteMeta(want) + `\b`).Match(recipe) {
			t.Errorf("%s does not create %s 0755 before installing into it", file, want)
		}
	}
}

// Every hook this image publishes names opencode as its provider, which is
// what the mapping is keyed by and what `discobox admin audit hooks --provider`
// selects on.
func TestPluginPublishesUnderTheDriverProvider(t *testing.T) {
	source, err := os.ReadFile(pluginSource)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `const PROVIDER = "`+Driver{}.ID()+`"`) {
		t.Errorf("plugin does not publish under the %s provider", Driver{}.ID())
	}
	if !strings.Contains(string(source), `spawn("discobox-hook-publish", ["--provider", PROVIDER, "--event", event]`) {
		t.Error("plugin does not publish through the generic publisher")
	}
}

// pluginPublishes runs the plugin against synthetic events and answers the
// event names it published, in order. It executes the plugin rather than
// reading it: the filtering this image depends on is four lines that a test
// grepping for constant names would not miss if they were deleted.
func pluginPublishes(t *testing.T) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's plugin runs on Linux, and the driver stubs its publisher with a shell script")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the plugin's behavior is not exercised")
	}
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "plugin-driver.mjs")).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("run plugin driver: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("run plugin driver: %v", err)
	}
	var published []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &published); err != nil {
		t.Fatalf("parse driver output %q: %v", out, err)
	}
	return published
}

// What the plugin does, not what it says. A child session's lifecycle is
// filtered and the root's is not; tool calls are published whichever session
// makes them; a stream-rate event and an unlisted one are published nowhere.
func TestPluginPublishesTheRootSessionsLifecycleAndEveryToolCall(t *testing.T) {
	got := pluginPublishes(t)
	want := []string{
		"session.created",             // the root's; the child's was dropped
		"session.execution.started",   // not filtered: it has no canonical name
		"tool.execute.before",         // tools are never filtered by session
		"tool.execute.after",          // this one is a child's
		"session.compaction.ended",    // the root's; the child's was dropped
		"session.execution.failed",    // the root's; the child's was dropped
		"session.execution.succeeded", // the root's; the child's was dropped
		"session.deleted",             // published, and forgets the child
		"session.execution.succeeded", // the forgotten child now reads as the root
	}
	if !slices.Equal(got, want) {
		t.Fatalf("published\n got %v\nwant %v", got, want)
	}
}

// rootPublishedCounts is how many times the driver's script should publish
// each session-scoped event: it fires every one of them for a child and for
// the root, so a count proves both that the child's copy is filtered and that
// the root's still gets through. A canonical name that stopped being published
// altogether would pass a "no child copy" check on its own.
var rootPublishedCounts = map[string]int{
	"session.created":             1,
	"session.execution.succeeded": 2, // the root's, and the child's after its deletion
	"session.execution.failed":    1,
	"session.compaction.ended":    1,
}

// Every session-scoped canonical name depends on the plugin filtering a
// child's copy of it, and "every" is meant literally: the expectations are
// checked against the plugin's own ROOT_ONLY_EVENTS, so adding a fifth
// filtered event with a canonical name fails here until it is covered.
func TestEveryCanonicalSessionEventIsFilteredForAChild(t *testing.T) {
	source, err := os.ReadFile(pluginSource)
	if err != nil {
		t.Fatal(err)
	}
	filtered := pluginSet(t, string(source), "ROOT_ONLY_EVENTS")
	published := pluginPublishes(t)

	for _, event := range filtered {
		if harness.CanonicalHookEvent(Driver{}.ID(), event) == "" {
			continue // unmapped, so a child's copy cannot be mistaken for a turn end
		}
		want, covered := rootPublishedCounts[event]
		if !covered {
			t.Errorf("%s is filtered and has a canonical name, but the driver never fires it", event)
			continue
		}
		if got := countOf(published, event); got != want {
			t.Errorf("%s published %d times, want %d — a child's copy is leaking, or the root's is lost", event, got, want)
		}
	}
	// And nothing in the expectations that the plugin no longer filters.
	for event := range rootPublishedCounts {
		if !slices.Contains(filtered, event) {
			t.Errorf("%s is expected to be root-only but the plugin no longer filters it", event)
		}
	}
}

func countOf(all []string, want string) int {
	n := 0
	for _, v := range all {
		if v == want {
			n++
		}
	}
	return n
}
