package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func TestToTUIHarnessState(t *testing.T) {
	cases := []struct {
		name string
		cfg  apimodel.HarnessConfig
		want tui.HarnessState
	}{
		{"configured", apimodel.HarnessConfig{Configured: true}, tui.HarnessEnabled},
		{"never configured", apimodel.HarnessConfig{}, tui.HarnessDisabled},
		{"configure failed", apimodel.HarnessConfig{ConfigureError: apiclientgen.NewOptString("boom")}, tui.HarnessFailed},
		// A reconfigure that failed after one that worked leaves the working
		// configuration in place, so the harness is still enabled.
		{"failed after configured", apimodel.HarnessConfig{Configured: true, ConfigureError: apiclientgen.NewOptString("boom")}, tui.HarnessEnabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := harnessState(tc.cfg); got != tc.want {
				t.Fatalf("harnessState = %q, want %q", got, tc.want)
			}
		})
	}
}

// A harness config becomes a harness row: its identity, whether it is the
// project default, and its files with the ones its configure flow wrote first,
// since those overlay the image-declared file of the same path.
func TestToTUIHarness(t *testing.T) {
	cfg := apimodel.HarnessConfig{
		ID: "hc_1", Name: "Claude", Slug: "claude", Configured: true, BuiltIn: true,
		Image:      apiclientgen.NewOptString("ghcr.io/example/claude:latest"),
		RunCommand: []string{"claude", "--dangerously-skip-permissions"},
		Secrets: apiclientgen.NewOptNilHarnessConfigSecretArray([]apimodel.HarnessConfigSecret{
			{Name: "ANTHROPIC_API_KEY", Required: apiclientgen.NewOptBool(true), OneOfGroup: apiclientgen.NewOptString("auth")},
		}),
		Files: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: "settings.json", Content: "{}"},
		}),
		ConfiguredFiles: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".claude.json", Content: "{}", CreateOnly: apiclientgen.NewOptBool(true)},
		}),
		UpdatedAt: time.Unix(10, 0),
	}

	harness := toTUIHarness(cfg, "hc_1")
	if harness.State != tui.HarnessEnabled || !harness.Default || !harness.BuiltIn {
		t.Fatalf("harness = %+v, want an enabled, default, built-in harness", harness)
	}
	if len(harness.Secrets) != 1 || harness.Secrets[0].Name != "ANTHROPIC_API_KEY" ||
		!harness.Secrets[0].Required || harness.Secrets[0].OneOf != "auth" || !harness.Secrets[0].Declared {
		t.Fatalf("secrets = %+v, want the declaration carried through", harness.Secrets)
	}
	if len(harness.Files) != 2 {
		t.Fatalf("files = %+v, want both sets", harness.Files)
	}
	if harness.Files[0].Path != ".claude.json" || !harness.Files[0].Configured || !harness.Files[0].CreateOnly {
		t.Fatalf("first file = %+v, want the configured .claude.json", harness.Files[0])
	}
	if harness.Files[1].Path != "settings.json" || harness.Files[1].Configured {
		t.Fatalf("second file = %+v, want the image-declared settings.json", harness.Files[1])
	}

	if other := toTUIHarness(cfg, "hc_other"); other.Default {
		t.Fatal("a harness that is not the project's default should not say it is")
	}
}

// The card's secrets are the image's declarations resolved against the
// bindings: what is bound, what is not, and the bindings the image never
// declared, which somebody bound by hand.
func TestHarnessSecretsResolveBindings(t *testing.T) {
	cfg := apimodel.HarnessConfig{
		Secrets: apiclientgen.NewOptNilHarnessConfigSecretArray([]apimodel.HarnessConfigSecret{
			{Name: "ANTHROPIC_API_KEY", Required: apiclientgen.NewOptBool(true)},
			{Name: "CLAUDE_CODE_OAUTH_TOKEN"},
		}),
	}
	bindings := []apimodel.HarnessConfigSecretBinding{
		{EnvName: "ANTHROPIC_API_KEY", SecretId: "sec_1"},
		{EnvName: "CUSTOM_TOKEN", SecretId: "sec_2"},
	}
	secretsByID := map[string]apimodel.Secret{
		"sec_1": {ID: "sec_1", Name: "anthropic-api-key", Type: apiclientgen.SecretType("api_key")},
	}

	got := harnessSecrets(cfg, bindings, secretsByID)
	if len(got) != 3 {
		t.Fatalf("secrets = %+v, want both declarations and the undeclared binding", got)
	}
	if got[0].SecretID != "sec_1" || got[0].SecretType != "api_key" || got[0].SecretName != "anthropic-api-key" {
		t.Fatalf("bound declaration = %+v, want the assigned secret described", got[0])
	}
	if got[1].Name != "CLAUDE_CODE_OAUTH_TOKEN" || got[1].SecretID != "" {
		t.Fatalf("unbound declaration = %+v, want nothing bound", got[1])
	}
	if got[2].Name != "CUSTOM_TOKEN" || got[2].Declared || got[2].SecretID != "sec_2" {
		t.Fatalf("undeclared binding = %+v, want it listed as bound by hand", got[2])
	}
	// The secret is not in the project listing, so only its id is known.
	if got[2].SecretType != "" || got[2].SecretName != "" {
		t.Fatalf("undeclared binding = %+v, want an id with nothing to describe it", got[2])
	}
}

// Disabling the project default must release the default first: the server
// refuses to deconfigure a default harness, so the data source does the unset
// the user would otherwise have to do by hand.
func TestDoHarnessDisableReleasesTheDefaultFirst(t *testing.T) {
	const harnessID = "hc_1"
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"defaultHarnessConfigId":"` + harnessID + `","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/default":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/deconfigure":
			_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"codex","name":"Codex","runCommand":["codex"],"configured":false,"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	if err := ds.DoHarness(context.Background(), tui.HarnessDisable, harnessID); err != nil {
		t.Fatalf("disable the default harness: %v", err)
	}

	want := []string{
		"GET /projects/project-1",
		"DELETE /projects/project-1/harness-configs/" + harnessID + "/default",
		"POST /projects/project-1/harness-configs/" + harnessID + "/deconfigure",
	}
	if len(gotPaths) != len(want) {
		t.Fatalf("requests = %v, want %v", gotPaths, want)
	}
	for i, path := range want {
		if gotPaths[i] != path {
			t.Fatalf("requests = %v, want %v", gotPaths, want)
		}
	}
}

// A harness that is not the default is deconfigured without touching it.
func TestDoHarnessDisableKeepsAnotherDefault(t *testing.T) {
	const harnessID = "hc_1"
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"defaultHarnessConfigId":"hc_other","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/deconfigure":
			_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"codex","name":"Codex","runCommand":["codex"],"configured":false,"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	if err := ds.DoHarness(context.Background(), tui.HarnessDisable, harnessID); err != nil {
		t.Fatalf("disable a harness: %v", err)
	}
	for _, path := range gotPaths {
		if path == "DELETE /projects/project-1/harness-configs/"+harnessID+"/default" {
			t.Fatalf("requests = %v, want no unset of a default this harness does not hold", gotPaths)
		}
	}
}
