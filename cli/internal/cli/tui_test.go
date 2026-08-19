package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// The launcher draws five states, so the server's fuller set has to land on one
// of them — and a transitional state has to land on the one that answers "can I
// act on this", which is where it is heading, not where it came from.
func TestToTUISandboxNarrowsDisplayState(t *testing.T) {
	for _, tc := range []struct {
		display string
		want    tui.State
	}{
		{"running", tui.StateRunning},
		{"starting", tui.StateStarting},
		{"stopping", tui.StateRunning},
		{"stopped", tui.StateStopped},
		{"archiving", tui.StateArchived},
		{"archived", tui.StateArchived},
		{"error", tui.StateError},
	} {
		sandbox := toTUISandbox(apimodel.Sandbox{
			Runtime: apimodel.SandboxRuntime{
				State:        "running",
				DesiredState: "present",
				DisplayState: apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayState(tc.display)),
			},
		})
		if sandbox.State != tc.want {
			t.Errorf("display %q -> %q, want %q", tc.display, sandbox.State, tc.want)
		}
	}
}

// The starred commit on a row means the sandbox was cut from a snapshot of
// uncommitted work, which is recorded as the source's snapshot ref.
func TestToTUISandboxMarksSnapshotSources(t *testing.T) {
	source := apimodel.GitSource{Kind: "git"}
	source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(apiclientgen.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString("a3f9c2179bbf0f4e2e9d1a7c5b6d8e0f11223344"),
		RefName: apiclientgen.NewOptString("main"),
		RefType: apiclientgen.NewOptString("branch"),
	}))
	source.SetWorkspace(apiclientgen.NewOptGitSourceWorkspace(apiclientgen.GitSourceWorkspace{
		Mode:        apiclientgen.NewOptGitSourceWorkspaceMode("dirty"),
		SnapshotRef: apiclientgen.NewOptString("refs/disco/snapshot"),
	}))
	sandbox := apimodel.Sandbox{Runtime: apimodel.SandboxRuntime{State: "running", DesiredState: "present"}}
	sandbox.Config.SetSource(apiclientgen.NewOptGitSource(source))

	row := toTUISandbox(sandbox)
	if row.Branch != "main" || row.Commit != "a3f9c21" {
		t.Fatalf("base = %q@%q, want main@a3f9c21", row.Branch, row.Commit)
	}
	if !row.Dirty {
		t.Fatal("a snapshot source should mark the row dirty")
	}
}

// The ref half of -C DIR@REF says which commit to cut from; the working tree
// being asked about is the directory's either way.
func TestSourceDirectoryDropsTheRef(t *testing.T) {
	for source, want := range map[string]string{
		"":                     ".",
		"/src/disco2":          "/src/disco2",
		"/src/disco2@main":     "/src/disco2",
		"/src/disco2@HEAD~1":   "/src/disco2",
		"@main":                "@main",
		"https://x/y.git@main": "https://x/y.git",
	} {
		if got := sourceDirectory(source); got != want {
			t.Errorf("sourceDirectory(%q) = %q, want %q", source, got, want)
		}
	}
}

// Run is the launcher's Enter, and it has to go through the same creation path
// `disco run` does rather than posting a body of its own.
func TestAPIDataSourceRunUsesSharedRunCreation(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","displayName":"tui-test","config":{"name":"tui-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{
		app:       &App{serverURL: server.URL},
		client:    client,
		projectID: "project-1",
	}
	sandbox, err := ds.Run(t.Context(), tui.RunRequest{
		Harness: "codex",
		Source:  repo + "@HEAD",
		Prompt:  "fix the failing tests",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sandbox.ID != "sbx_9qk5n25t2hh2rv00" {
		t.Fatalf("sandbox ID = %q", sandbox.ID)
	}
	if posted["harnessName"] != "codex" {
		t.Fatalf("harnessName = %#v, want codex", posted["harnessName"])
	}
	config := posted["config"].(map[string]any)
	prompt := config["prompt"].([]any)
	if len(prompt) != 1 || prompt[0] != "fix the failing tests" {
		t.Fatalf("prompt = %#v", prompt)
	}
	source := config["source"].(map[string]any)
	checkout := source["checkout"].(map[string]any)
	if checkout["commit"] != commit || checkout["refType"] != "commit" {
		t.Fatalf("checkout = %#v, want HEAD commit %s", checkout, commit)
	}
}

// A pane's shell is told how much color to use, from the terminal this window
// is running in. Unset names are dropped, so a sandbox only ever hears about a
// setting somebody made.
func TestPaneTerminalEnvCarriesTheColorSettings(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	os.Unsetenv("NO_COLOR")

	env, err := keyValueMapFromShell(paneTerminalEnv())
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if env["COLORTERM"] != "truecolor" {
		t.Fatalf("COLORTERM = %q, want the one this shell has", env["COLORTERM"])
	}
	if _, ok := env["NO_COLOR"]; ok {
		t.Fatal("an unset name should not be sent at all")
	}

	// And TERM is not among them: the terminal on this side of a pane is an
	// emulator, not yours, and the sandbox's own default describes it. A TERM
	// the sandbox has no terminfo for is how "unknown terminal type" happens.
	if _, ok := env["TERM"]; ok {
		t.Fatal("TERM should not be forwarded into a pane")
	}

	t.Setenv("NO_COLOR", "1")
	env, err = keyValueMapFromShell(paneTerminalEnv())
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if env["NO_COLOR"] != "1" {
		t.Fatalf("NO_COLOR = %q, want it carried across", env["NO_COLOR"])
	}
}
