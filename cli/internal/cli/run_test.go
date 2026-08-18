package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
)

func TestParseRunOptions(t *testing.T) {
	opts, err := sandboxcreate.ParsePromptOptions(sandboxcreate.PromptOptions{Source: "https://example.com/repo.git@main"}, []string{"fix", "tests"})
	if err != nil {
		t.Fatalf("parseRunOptions: %v", err)
	}
	if opts.Source != "https://example.com/repo.git" || opts.Ref != "main" {
		t.Fatalf("source/ref = %q/%q, want repo/main", opts.Source, opts.Ref)
	}
	if len(opts.Prompt) != 2 || opts.Prompt[0] != "fix" || opts.Prompt[1] != "tests" {
		t.Fatalf("prompt = %#v, want fix tests", opts.Prompt)
	}
}

func TestParseRunOptionsKeepsSSHRepoWithoutRef(t *testing.T) {
	opts, err := sandboxcreate.ParsePromptOptions(sandboxcreate.PromptOptions{Source: "git@github.com:obot-platform/discobox.git"}, []string{"fix"})
	if err != nil {
		t.Fatalf("parseRunOptions: %v", err)
	}
	if opts.Source != "git@github.com:obot-platform/discobox.git" || opts.Ref != "" {
		t.Fatalf("source/ref = %q/%q, want SSH repo with empty ref", opts.Source, opts.Ref)
	}
}

func TestParseRunOptionsSplitsSSHRepoWithRef(t *testing.T) {
	opts, err := sandboxcreate.ParsePromptOptions(sandboxcreate.PromptOptions{Source: "git@github.com:obot-platform/discobox.git@main"}, []string{"fix"})
	if err != nil {
		t.Fatalf("parseRunOptions: %v", err)
	}
	if opts.Source != "git@github.com:obot-platform/discobox.git" || opts.Ref != "main" {
		t.Fatalf("source/ref = %q/%q, want SSH repo/main", opts.Source, opts.Ref)
	}
}

func TestParseRunOptionsRejectsEmptySource(t *testing.T) {
	if _, err := sandboxcreate.ParsePromptOptions(sandboxcreate.PromptOptions{Source: " "}, []string{"fix"}); err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestRunCommandCreatesSandbox(t *testing.T) {
	t.Setenv("RUN_ENV_FROM_SHELL", "from-shell")
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
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
		_, _ = w.Write([]byte(`{"id":"` + sandboxID + `","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-d", "-e", "EXPLICIT=value", "-e", "RUN_ENV_FROM_SHELL", "-C", repo + "@HEAD", "fix", "tests"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	config := posted["config"].(map[string]any)
	nameParts := strings.Split(config["name"].(string), "_")
	if len(nameParts) != 2 || nameParts[0] == "" || nameParts[1] == "" {
		t.Fatalf("name = %q, want adjective_name", config["name"])
	}
	prompt, ok := config["prompt"].([]any)
	if !ok || len(prompt) != 2 || prompt[0] != "fix" || prompt[1] != "tests" {
		t.Fatalf("prompt = %#v, want [fix tests]", config["prompt"])
	}
	env, ok := config["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %#v, want object", config["env"])
	}
	if env["EXPLICIT"] != "value" || env["RUN_ENV_FROM_SHELL"] != "from-shell" {
		t.Fatalf("env = %#v, want explicit and shell values", env)
	}
	source, ok := config["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %#v, want object", config["source"])
	}
	if source["localDirectory"] != repo {
		t.Fatalf("localDirectory = %q, want %s", source["localDirectory"], repo)
	}
	checkout := source["checkout"].(map[string]any)
	if checkout["commit"] != commit || checkout["refType"] != "commit" {
		t.Fatalf("checkout = %#v, want commit %s", checkout, commit)
	}
	destination := source["destination"].(map[string]any)
	if destination["directory"] != repo || destination["workingDirectory"] != repo {
		t.Fatalf("destination = %#v, want repo root", destination)
	}
	workspace := source["workspace"].(map[string]any)
	if workspace["mode"] != "clean" {
		t.Fatalf("workspace = %#v, want clean", workspace)
	}
	if output := out.String(); !strings.Contains(output, sandboxID) {
		t.Fatalf("output = %q, want sandbox ID", output)
	}
}

func TestRunCommandDefaultsSourceToCurrentDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)
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
		_, _ = w.Write([]byte(`{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-d", "fix", "tests"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	config := posted["config"].(map[string]any)
	source, ok := config["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %#v, want object", config["source"])
	}
	if source["localDirectory"] != repo {
		t.Fatalf("localDirectory = %q, want %s", source["localDirectory"], repo)
	}
	destination := source["destination"].(map[string]any)
	if destination["directory"] != repo || destination["workingDirectory"] != repo {
		t.Fatalf("destination = %#v, want repo root", destination)
	}
	prompt, ok := config["prompt"].([]any)
	if !ok || len(prompt) != 2 || prompt[0] != "fix" || prompt[1] != "tests" {
		t.Fatalf("prompt = %#v, want [fix tests]", config["prompt"])
	}
}

func TestRunCommandStillAcceptsDashDashSeparator(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		sawCreate = true
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-d", "-C", repo + "@HEAD", "--", "hello", "--flag-like", "prompt"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if !sawCreate {
		t.Fatal("expected sandbox create request")
	}
}

func TestCreateRunSandboxBodyUsesSplitSourceRef(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))

	body, err := sandboxcreate.BuildPromptSandboxBody(t.Context(), sandboxcreate.PromptOptions{
		Source: repo + "@HEAD",
		Prompt: []string{"hello", "world"},
		Env:    []string{"RUN_BODY_ENV=value"},
	})
	if err != nil {
		t.Fatalf("createRunSandboxBody: %v", err)
	}
	if len(body.Config.Prompt) != 2 || body.Config.Prompt[0] != "hello" || body.Config.Prompt[1] != "world" {
		t.Fatalf("prompt = %#v, want [hello world]", body.Config.Prompt)
	}
	env, ok := body.Config.Env.Get()
	if !ok || env["RUN_BODY_ENV"] != "value" {
		t.Fatalf("env = %#v ok=%t, want RUN_BODY_ENV", env, ok)
	}
	source, ok := body.Config.Source.Get()
	if !ok {
		t.Fatal("expected source")
	}
	if source.LocalDirectory.Value != filepath.Clean(repo) {
		t.Fatalf("localDirectory = %q, want %s", source.LocalDirectory.Value, repo)
	}
	checkout, ok := source.Checkout.Get()
	if !ok || checkout.Commit.Value != commit || checkout.RefType.Value != "commit" {
		t.Fatalf("checkout = %#v ok=%t, want commit %s", checkout, ok, commit)
	}
}

func TestCreateRunSandboxBodySecrets(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	body, err := sandboxcreate.BuildPromptSandboxBody(t.Context(), sandboxcreate.PromptOptions{
		Source: repo,
		Env:    []string{"PLAIN=value"},
		Secret: []string{"OPENAI_API_KEY=sk-secret", "GITHUB_TOKEN=<sec_123>"},
	})
	if err != nil {
		t.Fatalf("createRunSandboxBody: %v", err)
	}
	env, ok := body.Config.Env.Get()
	if !ok || env["PLAIN"] != "value" {
		t.Fatalf("env = %#v ok=%t, want PLAIN", env, ok)
	}
	if len(body.Config.Secrets) != 2 {
		t.Fatalf("secrets = %#v, want 2 entries", body.Config.Secrets)
	}
	byEnv := map[string]apimodel.SandboxSecretInput{}
	for _, s := range body.Config.Secrets {
		byEnv[s.Env] = s
	}
	if got := byEnv["OPENAI_API_KEY"]; got.Value.Value != "sk-secret" || got.SecretId.Set {
		t.Fatalf("OPENAI_API_KEY = %#v, want inline value", got)
	}
	if got := byEnv["GITHUB_TOKEN"]; got.SecretId.Value != "sec_123" || got.Value.Set {
		t.Fatalf("GITHUB_TOKEN = %#v, want secret reference", got)
	}
}

// run must attach the sandbox-agent's virtual primary id, not the concrete id
// it polled for: attaching a concrete id addresses one session, so a primary
// that ended before or during the attach would fail instead of being relaunched.
func TestRunCommandAttachesVirtualPrimaryTerminal(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	var attachPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/sandboxes":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, "pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes/"+sandboxID:
			_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, "running")))
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1/sandboxes/"+sandboxID+"/execs":
			_, _ = w.Write([]byte(`{"execs":[{"id":"ex_primaryterminal000","harnessId":"codex","primary":true,"status":"running","command":["codex"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			attachPath = r.URL.Path
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"session is no longer available to attach"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-C", repo + "@HEAD", "fix", "tests"})

	err := cmd.Execute()
	t.Logf("run err = %v", err)
	if err == nil {
		t.Fatal("execute run error = nil, want the rejected attach")
	}
	if want := "/api/projects/project-1/sandboxes/" + sandboxID + "/execs/primary/attach"; attachPath != want {
		t.Fatalf("attach path = %q, want %q", attachPath, want)
	}
}

// runTestSandboxJSON builds a sandbox response in the phase the caller names,
// filling in both state axes the way the server would (ADR 0034) plus the
// derived displayState the CLI actually waits on — a fixture that omitted it
// would leave the wait loop spinning forever.
//
// The phases that are not existence states are runtime ones, and a sandbox with
// a runtime state has necessarily converged, so they come back as `ready` plus
// the observation.
func runTestSandboxJSON(sandboxID, phase string) string {
	state, runtimeState, displayState := phase, "", phase
	switch phase {
	case "pending", "awaiting_source":
		displayState = "starting"
	case "failed":
		displayState = "error"
	default:
		state, runtimeState = "ready", phase
	}
	runtime := `"state":"` + state + `","displayState":"` + displayState + `","desiredState":"present","generation":1,"observedGeneration":1`
	if runtimeState != "" {
		runtime += `,"runtimeState":"` + runtimeState + `"`
	}
	return `{"id":"` + sandboxID + `","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{` + runtime + `},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`
}
