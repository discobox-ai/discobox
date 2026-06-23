package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRunArgs(t *testing.T) {
	opts, err := parseRunArgs([]string{"https://example.com/repo.git@main", "fix", "tests"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.source != "https://example.com/repo.git" || opts.ref != "main" {
		t.Fatalf("source/ref = %q/%q, want repo/main", opts.source, opts.ref)
	}
	if len(opts.prompt) != 2 || opts.prompt[0] != "fix" || opts.prompt[1] != "tests" {
		t.Fatalf("prompt = %#v, want fix tests", opts.prompt)
	}
}

func TestParseRunArgsKeepsSSHRepoWithoutRef(t *testing.T) {
	opts, err := parseRunArgs([]string{"git@github.com:obot-platform/discobox.git", "fix"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.source != "git@github.com:obot-platform/discobox.git" || opts.ref != "" {
		t.Fatalf("source/ref = %q/%q, want SSH repo with empty ref", opts.source, opts.ref)
	}
}

func TestParseRunArgsSplitsSSHRepoWithRef(t *testing.T) {
	opts, err := parseRunArgs([]string{"git@github.com:obot-platform/discobox.git@main", "fix"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	if opts.source != "git@github.com:obot-platform/discobox.git" || opts.ref != "main" {
		t.Fatalf("source/ref = %q/%q, want SSH repo/main", opts.source, opts.ref)
	}
}

func TestRunCommandCreatesSandbox(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	const sandboxID = "01kv9w440bpa9qk5n25t2hh2rv"
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
		_, _ = w.Write([]byte(`{"id":"` + sandboxID + `","projectId":"project-1","createdByUserId":"user-1","name":"run-test","phase":"pending","desiredState":"stopped","lastOperationStatus":"pending","generation":1,"observedGeneration":0,"restartGeneration":0,"restartedGeneration":0,"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", repo + "@HEAD", "fix", "tests"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if !strings.HasPrefix(posted["name"].(string), "run-") {
		t.Fatalf("name = %q, want run-*", posted["name"])
	}
	if posted["prompt"] != "fix tests" {
		t.Fatalf("prompt = %q, want fix tests", posted["prompt"])
	}
	source, ok := posted["source"].(map[string]any)
	if !ok {
		t.Fatalf("source = %#v, want object", posted["source"])
	}
	if source["localDirectory"] != repo {
		t.Fatalf("localDirectory = %q, want %s", source["localDirectory"], repo)
	}
	checkout := source["checkout"].(map[string]any)
	if checkout["commit"] != commit || checkout["refType"] != runSourceRefTypeCommit {
		t.Fatalf("checkout = %#v, want commit %s", checkout, commit)
	}
	destination := source["destination"].(map[string]any)
	if destination["directory"] != repo || destination["workingDirectory"] != repo {
		t.Fatalf("destination = %#v, want repo root", destination)
	}
	workspace := source["workspace"].(map[string]any)
	if workspace["mode"] != runWorkspaceModeClean {
		t.Fatalf("workspace = %#v, want clean", workspace)
	}
	if output := out.String(); !strings.Contains(output, shortID(sandboxID)) {
		t.Fatalf("output = %q, want sandbox ID", output)
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
		_, _ = w.Write([]byte(`{"id":"01kv9w440bpa9qk5n25t2hh2rv","projectId":"project-1","createdByUserId":"user-1","name":"run-test","phase":"pending","desiredState":"stopped","lastOperationStatus":"pending","generation":1,"observedGeneration":0,"restartGeneration":0,"restartedGeneration":0,"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "--", repo + "@HEAD", "hello"})

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

	body, err := createRunSandboxBody(t.Context(), runOptions{
		source: repo,
		ref:    "HEAD",
		prompt: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("createRunSandboxBody: %v", err)
	}
	if body.Prompt.Value != "hello world" {
		t.Fatalf("prompt = %q, want hello world", body.Prompt.Value)
	}
	source, ok := body.Source.Get()
	if !ok {
		t.Fatal("expected source")
	}
	if source.LocalDirectory.Value != filepath.Clean(repo) {
		t.Fatalf("localDirectory = %q, want %s", source.LocalDirectory.Value, repo)
	}
	checkout, ok := source.Checkout.Get()
	if !ok || checkout.Commit.Value != commit || checkout.RefType.Value != runSourceRefTypeCommit {
		t.Fatalf("checkout = %#v ok=%t, want commit %s", checkout, ok, commit)
	}
}
