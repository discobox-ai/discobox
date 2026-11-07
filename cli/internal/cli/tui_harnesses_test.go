package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A harness config becomes a harness row: its identity, whether it is the
// project default, and its files with the ones saved from discoboxes first,
// since those overlay the image-declared file of the same path.
func TestToTUIHarness(t *testing.T) {
	cfg := apimodel.HarnessConfig{
		ID: "hc_1", Name: "Claude", Slug: "claude", BuiltIn: true,
		Bundle:     apiclientgen.NewOptString("ghcr.io/example/claude:latest"),
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
	if !harness.Default || !harness.BuiltIn {
		t.Fatalf("harness = %+v, want a default, built-in harness", harness)
	}
	if len(harness.Secrets) != 1 || harness.Secrets[0].Name != "ANTHROPIC_API_KEY" ||
		!harness.Secrets[0].Required || harness.Secrets[0].OneOf != "auth" || !harness.Secrets[0].Declared {
		t.Fatalf("secrets = %+v, want the declaration carried through", harness.Secrets)
	}
	if len(harness.Files) != 2 {
		t.Fatalf("files = %+v, want both sets", harness.Files)
	}
	if harness.Files[0].Path != ".claude.json" || harness.Files[0].Source != console.HarnessFileSaved || !harness.Files[0].CreateOnly {
		t.Fatalf("first file = %+v, want the saved .claude.json", harness.Files[0])
	}
	if harness.Files[1].Path != "settings.json" || harness.Files[1].Source != console.HarnessFileFromBundle {
		t.Fatalf("second file = %+v, want the image-declared settings.json", harness.Files[1])
	}

	// A person's override of a path leads, naming the file it hides.
	cfg.FileOverrides = apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{{Path: "settings.json", Content: "mine"}})
	if files := toTUIHarness(cfg, "hc_1").Files; len(files) != 2 || files[0].Path != "settings.json" ||
		files[0].Source != console.HarnessFileOverride || len(files[0].Shadows) != 1 || files[0].Shadows[0] != console.HarnessFileFromBundle {
		t.Fatalf("files = %+v, want the override of settings.json first, over the bundle's", files)
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
	bindings := []apimodel.SecretBinding{
		{EnvVar: apiclientgen.NewOptString("ANTHROPIC_API_KEY"), SecretId: "sec_1", Use: true},
		{EnvVar: apiclientgen.NewOptString("CUSTOM_TOKEN"), SecretId: "sec_2", Use: true},
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

// Reset is the reset route and nothing else: the project default stays the
// default, since a reset harness can still run.
func TestDoHarnessResetCallsReset(t *testing.T) {
	const harnessID = "hc_1"
	var gotPaths []string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/reset" {
			_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"codex","name":"Codex","runCommand":["codex"],"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	if err := ds.DoHarness(context.Background(), "", console.HarnessReset, harnessID); err != nil {
		t.Fatalf("reset a harness: %v", err)
	}
	if want := "POST /projects/project-1/harness-configs/" + harnessID + "/reset"; len(gotPaths) != 1 || gotPaths[0] != want {
		t.Fatalf("requests = %v, want only %s", gotPaths, want)
	}
}

func TestCreateHarnessVariantUsesSelectedServersCreateAPI(t *testing.T) {
	var calls int
	httpServer := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/projects/work-project/harness-configs" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body) != 3 || body["extends"] != "parent-id" || body["name"] != "Work" || body["slug"] != "work" {
			t.Errorf("body %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"new-id","extends":"parent-id","projectId":"work-project","slug":"work","name":"Work","runCommand":["codex"],"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	defer httpServer.Close()
	client, err := apiclientgen.NewClient(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	work := &apiDataSource{app: &App{}, client: client, projectID: "work-project"}
	ds := &apiDataSource{app: &App{}, projectID: "wrong-project", servers: []*tuiServer{{server: &server{name: "work-server"}, source: work}}}
	got, err := ds.CreateHarnessVariant(context.Background(), "work-server", console.NewHarnessVariant{ParentID: "parent-id", Name: "Work", Slug: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.ID != "new-id" || got.Parent != "parent-id" {
		t.Fatalf("calls=%d harness=%+v", calls, got)
	}
}
