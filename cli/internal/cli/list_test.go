package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSourceRootUsesRepoRootForSubdirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	root, err := resolveSourceRoot(context.Background(), ".")
	if err != nil {
		t.Fatalf("resolveSourceRoot: %v", err)
	}
	if root != repo {
		t.Fatalf("source root = %q, want %q", root, repo)
	}
}

func TestResolveSourceRootDropsRefFromRemoteURL(t *testing.T) {
	root, err := resolveSourceRoot(context.Background(), "https://github.com/obot-platform/discobox.git@main")
	if err != nil {
		t.Fatalf("resolveSourceRoot: %v", err)
	}
	if want := "https://github.com/obot-platform/discobox.git"; root != want {
		t.Fatalf("source root = %q, want %q", root, want)
	}
}

func TestResolveSourceRootOutsideRepositoryFails(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, err := resolveSourceRoot(context.Background(), "."); err == nil {
		t.Fatal("resolveSourceRoot outside a git repository: got nil error, want failure")
	}
}

func TestListCommandFiltersSandboxesByCurrentRepositoryRoot(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	var gotSourceRoot string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotSourceRoot = r.URL.Query().Get("sourceRoot")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	defer server.Close()

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ls: %v", err)
	}
	if gotSourceRoot != repo {
		t.Fatalf("sourceRoot query = %q, want %q", gotSourceRoot, repo)
	}
}
