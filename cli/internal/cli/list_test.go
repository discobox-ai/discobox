package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
	"github.com/obot-platform/discobox/internal/hostid"
	"github.com/obot-platform/discobox/internal/originkey"
)

func TestResolveOriginUsesRepoRootForSubdirectory(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	resolved, err := sandboxcreate.ResolveOrigin(context.Background(), ".")
	if err != nil {
		t.Fatalf("ResolveOrigin: %v", err)
	}
	if resolved.ProjectPath != repo {
		t.Fatalf("project path = %q, want repo root %q", resolved.ProjectPath, repo)
	}
	if resolved.HostId != "host_0123456789abcdef" {
		t.Fatalf("host ID = %q, want the override", resolved.HostId)
	}
}

// A remote source has no local project directory, so the origin is the
// directory the command ran from rather than the URL.
func TestResolveOriginForRemoteSourceUsesWorkingDirectory(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	resolved, err := sandboxcreate.ResolveOrigin(context.Background(), "https://github.com/obot-platform/discobox.git@main")
	if err != nil {
		t.Fatalf("ResolveOrigin: %v", err)
	}
	if resolved.ProjectPath != repo {
		t.Fatalf("project path = %q, want working directory repo root %q", resolved.ProjectPath, repo)
	}
}

// Outside a repository the directory itself is the project, so listing still
// works rather than failing the way source-root resolution did.
func TestResolveOriginOutsideRepositoryUsesDirectory(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	dir := t.TempDir()
	t.Chdir(dir)

	resolved, err := sandboxcreate.ResolveOrigin(context.Background(), ".")
	if err != nil {
		t.Fatalf("ResolveOrigin outside a git repository: %v", err)
	}
	// t.TempDir may hand back a symlinked path; compare what the OS resolves.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(resolved.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("project path = %q, want %q", got, want)
	}
}

func TestListCommandFiltersSandboxesByOriginKey(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	var gotOriginKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotOriginKey = r.URL.Query().Get("originKey")
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
	want := originkey.Of("host_0123456789abcdef", repo)
	if gotOriginKey != want {
		t.Fatalf("originKey query = %q, want %q", gotOriginKey, want)
	}
	if gotOriginKey == "" {
		t.Fatal("originKey query was empty; ls would list every sandbox in the project")
	}
}
