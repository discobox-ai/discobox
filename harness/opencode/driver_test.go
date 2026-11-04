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
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/featuretest"
	"github.com/discobox-ai/discobox/harness/internal/launchertest"
)

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
	// until a saved harness change returns its own copy — so it is rewritten on every
	// launch rather than kept from the first, or an edit would never land.
	if len(image.Harness.Files) != 1 || image.Harness.Files[0].Path != settingsPath ||
		image.Harness.Files[0].CreateOnly {
		t.Fatalf("manifest files = %+v, want only the settings file, plain", image.Harness.Files)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(image.Harness.Files[0].Content), &settings); err != nil {
		t.Fatalf("baseline settings are not JSON: %v", err)
	}
	if len(settings) != 2 || settings["webSearch"] != nil {
		t.Fatalf("baseline settings = %v, want the about line and opencode's own web search default", settings)
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
	feature := featuretest.Load(t, ".")
	if !feature.Runs(t, "npm install -g @opencode/cli") {
		t.Error("the feature does not install opencode's CLI")
	}
	for source, want := range map[string]string{
		"launch.sh":      "/usr/local/bin/" + harness.RunCommand,
		"credentials.sh": credentialsHelper,
	} {
		if got := feature.Installs(source); got != want {
			t.Errorf("the feature installs %s at %q, want %q", source, got, want)
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
// launcher runs it from.
const credentialsHelper = "/usr/local/libexec/discobox/opencode-credentials" //nolint:gosec // A path, not a credential.

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
	helper := filepath.Join(t.TempDir(), "credentials.sh")
	writeExecutable(t, helper, strings.ReplaceAll(readFile(t, "credentials.sh"), "/usr/local/libexec/discobox/opencode-capture", "true"))
	cmd := exec.CommandContext(t.Context(), "sh", helper, "sync")
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

// credentialsPath is the harness file that delivers the credentials,
// home-relative.
const credentialsPath = ".config/discobox/opencode-credentials.json" //nolint:gosec // A path, not a credential.

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

// withHelpers points a script at the helpers in this checkout rather than
// where the image installs them, each behind an executable of its own as the
// image installs it.
func withHelpers(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	for path, source := range map[string]string{credentialsHelper: "credentials.sh"} {
		abs, err := filepath.Abs(source)
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(dir, filepath.Base(path))
		raw, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, wrapper, strings.ReplaceAll(string(raw), "/usr/local/libexec/discobox/opencode-capture", "true"))
		script = strings.ReplaceAll(script, path, wrapper)
	}
	script = strings.ReplaceAll(script, "/usr/local/libexec/discobox/opencode-capture", "true")
	return script
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // A stub the script under test executes.
		t.Fatal(err)
	}
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
// user's global config and a project's — and which save-back's
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
	// directory, and only the feature says so. Every directory a file step
	// makes is 0755, so every user's opencode can read the plugin.
	feature := featuretest.Load(t, ".")
	for source, path := range map[string]string{
		"hook-plugin/server.js":    want + "/server.js",
		"hook-plugin/package.json": want + "/package.json",
		"managed-config.json":      "/etc/opencode/opencode.json",
	} {
		if got := feature.Installs(source); got != path {
			t.Errorf("the feature installs %s at %q, want %q", source, got, path)
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
