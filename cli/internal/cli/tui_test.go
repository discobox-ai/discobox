package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/tui"
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
		SnapshotRef: apiclientgen.NewOptString("refs/discobox/snapshot"),
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
// `discobox run` does rather than posting a body of its own.
func TestAPIDataSourceRunUsesSharedRunCreation(t *testing.T) {
	serveSSHSync := preparePromptCreateSSHSync(t)
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSSHSync(w, r) {
			return
		}
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
	// The window is a full-screen program on this terminal, so the app's error
	// stream is the one thing the create may not write to. It is given one here
	// that fails the test if anything lands on it.
	var stray strings.Builder
	ds := &apiDataSource{
		app:       &App{serverURL: server.URL, errOut: &stray},
		client:    client,
		projectID: "project-1",
	}
	var steps []string
	sandbox, err := ds.Run(t.Context(), tui.RunRequest{
		Harness: "codex",
		Source:  repo + "@HEAD",
		Prompt:  []string{"fix the failing tests"},
	}, func(step string) { steps = append(steps, step) })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The launcher's create says what it is doing on the way through, in the
	// same words `discobox run` uses, because both call the same creation path
	// (ADR 0060). A source the server can reach needs no push, so the delivery
	// reports nothing here.
	phases := []string{
		string(sandboxcreate.StepPreparingSource),
		string(sandboxcreate.StepCreating),
		"syncing SSH config",
	}
	if len(steps) < len(phases) || !slices.Equal(steps[:len(phases)], phases) {
		t.Fatalf("reported steps = %q, want them to open with %q", steps, phases)
	}
	// What the sync did on the user's behalf is reported rather than printed,
	// which is the whole point of reporting it: the key it enrolled and the
	// files it wrote have nowhere to go but the busy line, and written to a
	// stream they would be drawn across the window's frame.
	notes := strings.Join(steps[len(phases):], "\n")
	for _, want := range []string{"generated a new SSH key at ", "enrolled SSH key ", "wrote "} {
		if !strings.Contains(notes, want) {
			t.Fatalf("the SSH config sync did not report %q on the busy line: %q", want, steps)
		}
	}
	if stray.Len() > 0 {
		t.Fatalf("the create wrote to the terminal the window is drawn on: %q", stray.String())
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

// The row carries the power axis alongside the narrowed display state, because
// displayState folds it away: an errored sandbox reads `error` whatever its
// container is doing, and the launcher's attach guard needs the difference
// between a latched failure over a live container and one with nothing behind
// it. The API omits runtimeState until an agent has reported (ADR 0034 §2), so
// presence is the signal.
func TestToTUISandboxCarriesTheRuntimeAxis(t *testing.T) {
	errored := func(runtimeState string) tui.Sandbox {
		runtime := apimodel.SandboxRuntime{
			State:        "failed",
			DesiredState: "present",
			DisplayState: apiclientgen.NewOptSandboxRuntimeDisplayState("error"),
			ErrorMessage: apiclientgen.NewOptString("bind source pruned"),
		}
		if runtimeState != "" {
			runtime.RuntimeState = apiclientgen.NewOptSandboxRuntimeRuntimeState(
				apiclientgen.SandboxRuntimeRuntimeState(runtimeState))
		}
		return toTUISandbox(apimodel.Sandbox{Runtime: runtime})
	}

	live := errored("running")
	if live.State != tui.StateError {
		t.Fatalf("state = %q, want error: the x still shows", live.State)
	}
	if !live.HasRuntime {
		t.Fatal("a reported container should set HasRuntime")
	}

	if errored("").HasRuntime {
		t.Fatal("an unreported container should leave HasRuntime false")
	}
}

// The row carries both names: the one the server says to show, which is the
// primary terminal's title once the harness has set one, and the one the box is
// configured with, which the launcher's status line says under the cursor.
func TestToTUISandboxCarriesBothNames(t *testing.T) {
	named := func(display, configured string) tui.Sandbox {
		sb := apimodel.Sandbox{ID: "sbx_abc12345000000p3", DisplayName: display}
		sb.Config.Name = configured
		return toTUISandbox(sb)
	}

	titled := named("fix the reaper", "brave-otter")
	if titled.Name != "fix the reaper" || titled.ConfigName != "brave-otter" {
		t.Fatalf("row = %q / %q, want the title beside the configured name", titled.Name, titled.ConfigName)
	}

	// A name that is only whitespace is no name: the server trims before
	// falling back to the ID, so the row has to trim before saying it has one.
	unnamed := named("sbx_abc12345000000p3", "  ")
	if unnamed.ConfigName != "" {
		t.Fatalf("ConfigName = %q, want empty: a blank name is no name", unnamed.ConfigName)
	}
}

// `discobox run` and `discobox attach` open the window on the discobox, but
// only where there is a terminal to draw one on: a pipe, a script or CI gets
// the stream those commands have always been, the same rule bare `discobox`
// follows before opening the launcher.
func TestTheWindowNeedsATerminalToOpenOn(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&bytes.Buffer{})
	if canOpenWindow(cmd) {
		t.Fatal("a command wired to buffers has no terminal to draw a window on")
	}
}
