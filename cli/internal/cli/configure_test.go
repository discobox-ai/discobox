package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func configureTestModel(harnesses ...apimodel.HarnessConfig) *configureModel {
	m := &configureModel{harnesses: sortHarnesses(harnesses)}
	return m
}

func TestConfigureModelCursorMovement(t *testing.T) {
	m := configureTestModel(
		apimodel.HarnessConfig{ID: "h1", Name: "claude", CreatedAt: time.Unix(1, 0)},
		apimodel.HarnessConfig{ID: "h2", Name: "codex", CreatedAt: time.Unix(2, 0)},
	)

	// Up at the top is a no-op.
	m.handleKey(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.cursor)
	}

	m.handleKey(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Fatalf("cursor after down = %d, want 1", m.cursor)
	}

	// Down at the bottom is a no-op.
	m.handleKey(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.cursor != 1 {
		t.Fatalf("cursor at bottom = %d, want 1", m.cursor)
	}
}

func TestConfigureModelDisableRequiresEnabled(t *testing.T) {
	m := configureTestModel(apimodel.HarnessConfig{ID: "h1", Name: "claude", Configured: false, CreatedAt: time.Unix(1, 0)})

	m.beginDisable()
	if m.confirmDisable != nil {
		t.Fatal("beginDisable on an unconfigured harness should not arm a confirmation")
	}
	if m.busy {
		t.Fatal("model should not be busy after a no-op disable")
	}
	if m.status == "" {
		t.Fatal("expected an explanatory status after a no-op disable")
	}
}

func TestConfigureModelDisableConfirmationFlow(t *testing.T) {
	enabled := apimodel.HarnessConfig{ID: "h1", Name: "claude", Configured: true, CreatedAt: time.Unix(1, 0)}

	// 'd' arms the confirmation rather than disabling immediately.
	m := configureTestModel(enabled)
	m.handleKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if m.confirmDisable == nil {
		t.Fatal("pressing d on an enabled harness should arm the disable confirmation")
	}
	if m.busy {
		t.Fatal("arming a confirmation should not start the disable")
	}

	// Any key other than y cancels without disabling.
	m.handleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.confirmDisable != nil {
		t.Fatal("a non-y key should cancel the pending disable")
	}
	if m.busy {
		t.Fatal("canceling should not start the disable")
	}

	// 'y' confirms and starts the disable.
	m2 := configureTestModel(enabled)
	m2.handleKey(tea.KeyPressMsg{Code: 'd', Text: "d"})
	_, cmd := m2.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("confirming with y should return a disable command")
	}
	if m2.confirmDisable != nil {
		t.Fatal("confirming should clear the pending confirmation")
	}
	if !m2.busy {
		t.Fatal("confirming should mark the model busy")
	}
}

func TestConfigureModelSetDefaultRequiresEnabled(t *testing.T) {
	m := configureTestModel(apimodel.HarnessConfig{ID: "h1", Name: "claude", Configured: false, CreatedAt: time.Unix(1, 0)})

	if cmd := m.setDefaultSelected(); cmd != nil {
		t.Fatal("setDefaultSelected on an unconfigured harness should be a no-op")
	}
	if m.busy {
		t.Fatal("model should not be busy after a no-op set-default")
	}
}

// Disabling the project default must unset the default first, since the server
// refuses to deconfigure a default harness. The model does this automatically.
func TestConfigureModelDisableDefaultUnsetsFirst(t *testing.T) {
	const harnessID = "hc_1"
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/default":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/deconfigure":
			_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"codex","name":"Codex","runCommand":["codex"],"configured":false,"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	cfg := apimodel.HarnessConfig{ID: harnessID, Name: "codex", Configured: true, CreatedAt: time.Unix(1, 0)}
	m := &configureModel{
		ctx:       context.Background(),
		client:    client,
		projectID: "project-1",
		harnesses: []apimodel.HarnessConfig{cfg},
		defaultID: harnessID,
	}

	cmd := m.runDisable(cfg)
	if cmd == nil {
		t.Fatal("runDisable returned no command")
	}
	msg, ok := cmd().(configureActionMsg)
	if !ok {
		t.Fatalf("message type = %T, want configureActionMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("disable default: %v", msg.err)
	}

	want := []string{
		"DELETE /projects/project-1/harness-configs/" + harnessID + "/default",
		"POST /projects/project-1/harness-configs/" + harnessID + "/deconfigure",
	}
	if len(gotPaths) != len(want) || gotPaths[0] != want[0] || gotPaths[1] != want[1] {
		t.Fatalf("requests = %v, want unset-default then deconfigure %v", gotPaths, want)
	}
}

// The footer must stay a constant height whether or not a status is showing, so
// the list above it never shifts. It is always exactly two trailing lines.
func TestConfigureModelFooterHeightIsStable(t *testing.T) {
	m := configureTestModel(apimodel.HarnessConfig{ID: "h1", Name: "codex", CreatedAt: time.Unix(1, 0)})

	noStatus := m.View().Content

	m.status = "enable codex before making it the default"
	m.statusIsError = true
	withError := m.View().Content

	if a, b := lineCount(noStatus), lineCount(withError); a != b {
		t.Fatalf("view height changed with status: no-status=%d with-error=%d", a, b)
	}
	msg, _ := m.footerLines()
	if !strings.Contains(msg, "✗") || !strings.Contains(msg, "enable codex") {
		t.Fatalf("error footer = %q, want a ✗-prefixed error bar", msg)
	}
}

func lineCount(s string) int {
	return strings.Count(s, "\n")
}

// 'v' prints the highlighted agent's config as a formatted card rather than raw
// JSON: identity, state, commands, secrets with their assigned secret (ID and
// type), and files including their contents.
func TestConfigureModelViewConfig(t *testing.T) {
	cfg := apimodel.HarnessConfig{
		ID:         "hc_1",
		Name:       "Claude",
		Slug:       "claude",
		Configured: true,
		BuiltIn:    true,
		Image:      apiclientgen.NewOptString("ghcr.io/example/claude:latest"),
		RunCommand: []string{"claude", "--dangerously-skip-permissions"},
		Secrets: apiclientgen.NewOptNilHarnessConfigSecretArray([]apimodel.HarnessConfigSecret{
			{Name: "ANTHROPIC_API_KEY", Required: apiclientgen.NewOptBool(true), OneOfGroup: apiclientgen.NewOptString("auth")},
			{Name: "CLAUDE_CODE_OAUTH_TOKEN", OneOfGroup: apiclientgen.NewOptString("auth")},
		}),
		ConfiguredFiles: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".claude.json", Content: `{"hasCompletedOnboarding":true}`, CreateOnly: apiclientgen.NewOptBool(true)},
		}),
		CreatedAt: time.Unix(1, 0),
	}
	bindings := []apimodel.HarnessConfigSecretBinding{
		{EnvName: "ANTHROPIC_API_KEY", SecretId: "sec_1"},
		{EnvName: "CUSTOM_TOKEN", SecretId: "sec_2"},
	}
	secretsByID := map[string]apimodel.Secret{
		"sec_1": {ID: "sec_1", Name: "anthropic-api-key", Type: apiclientgen.SecretType("api_key")},
	}
	m := configureTestModel(cfg)
	m.defaultID = "hc_1"

	_, cmd := m.handleKey(tea.KeyPressMsg{Code: 'v', Text: "v"})
	if cmd == nil {
		t.Fatal("pressing v should return a print command")
	}
	if !m.busy {
		t.Fatal("pressing v should mark the model busy while bindings load")
	}

	out := renderHarnessConfigDetail(cfg, true, bindings, secretsByID)
	for _, want := range []string{
		"Claude", "hc_1", "enabled", "default", "built-in",
		"ghcr.io/example/claude:latest",
		"claude --dangerously-skip-permissions",
		// Declared and bound: shows the assigned secret's ID, type, and name.
		"ANTHROPIC_API_KEY", "required", "one of auth", "sec_1", "api_key", "anthropic-api-key",
		// Declared but unbound.
		"CLAUDE_CODE_OAUTH_TOKEN", "not bound",
		// Bound but not declared by the image; its secret is not in the project
		// list, so only the ID shows.
		"CUSTOM_TOKEN", "undeclared", "sec_2",
		// Files print their contents, not just sizes.
		".claude.json", "create-only", `{"hasCompletedOnboarding":true}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config detail missing %q:\n%s", want, out)
		}
	}
	if json.Valid([]byte(out)) {
		t.Fatalf("config detail should not be raw JSON:\n%s", out)
	}
}

// 'f' opens the file picker for agents with files and reports an error for
// agents without any; esc backs out without editing.
func TestConfigureModelFilePick(t *testing.T) {
	bare := apimodel.HarnessConfig{ID: "h1", Name: "codex", CreatedAt: time.Unix(1, 0)}
	m := configureTestModel(bare)
	m.handleKey(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if m.filePick != nil {
		t.Fatal("f on an agent without files should not enter file-pick mode")
	}
	if !m.statusIsError {
		t.Fatal("expected an explanatory error status for an agent without files")
	}

	withFiles := apimodel.HarnessConfig{
		ID: "h2", Name: "claude", CreatedAt: time.Unix(1, 0),
		ConfiguredFiles: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".claude.json", Content: "{}"},
		}),
		Files: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: "settings.json", Content: "{}"},
		}),
	}
	m = configureTestModel(withFiles)
	m.handleKey(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if m.filePick == nil {
		t.Fatal("f should enter file-pick mode when the agent has files")
	}
	if len(m.filePick.files) != 2 || m.filePick.files[0].Path != ".claude.json" {
		t.Fatalf("filePick files = %+v, want configured files first", m.filePick.files)
	}
	view := m.View().Content
	if !strings.Contains(view, ".claude.json") || !strings.Contains(view, "settings.json") {
		t.Fatalf("file-pick view should list the files:\n%s", view)
	}

	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.filePick != nil {
		t.Fatal("esc should leave file-pick mode")
	}

	m.handleKey(tea.KeyPressMsg{Code: 'f', Text: "f"})
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in file-pick mode should return an edit command")
	}
	if m.filePick != nil {
		t.Fatal("starting an edit should leave file-pick mode")
	}
}

func TestConfigureDisplayNameFallback(t *testing.T) {
	cases := []struct {
		cfg  apimodel.HarnessConfig
		want string
	}{
		{apimodel.HarnessConfig{Name: "Claude", Slug: "claude", ID: "h1"}, "Claude"},
		{apimodel.HarnessConfig{Slug: "codex", ID: "h2"}, "codex"},
		{apimodel.HarnessConfig{ID: "h3"}, "h3"},
	}
	for _, tc := range cases {
		if got := configureDisplayName(tc.cfg); got != tc.want {
			t.Fatalf("configureDisplayName(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}
