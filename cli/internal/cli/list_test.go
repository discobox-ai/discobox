package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/internal/originkey"
)

// A directory's discoboxes are filed under its repository root, so ls from a
// subdirectory lists what was cut from the repository.
func TestSourceRootIsTheRepositoryRootForASubdirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	subdir := filepath.Join(repo, "nested", "work")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	root, err := sandboxcreate.SourceRoot(context.Background(), ".")
	if err != nil {
		t.Fatalf("SourceRoot: %v", err)
	}
	if root != repo {
		t.Fatalf("source root = %q, want repo root %q", root, repo)
	}
}

// A remote source is filed under its URL, as the create request carries it —
// with the ref dropped, since every ref of a repository is one place (ADR
// 0111).
func TestSourceRootForARemoteSourceIsItsURL(t *testing.T) {
	root, err := sandboxcreate.SourceRoot(context.Background(), "https://github.com/discobox-ai/discobox.git@main")
	if err != nil {
		t.Fatalf("SourceRoot: %v", err)
	}
	if want := "https://github.com/discobox-ai/discobox.git"; root != want {
		t.Fatalf("source root = %q, want %q", root, want)
	}
}

// Outside a repository the directory itself is the place, so listing still
// works there rather than failing.
func TestSourceRootOutsideARepositoryIsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	root, err := sandboxcreate.SourceRoot(context.Background(), ".")
	if err != nil {
		t.Fatalf("SourceRoot outside a git repository: %v", err)
	}
	// t.TempDir may hand back a symlinked path; compare what the OS resolves.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("source root = %q, want %q", got, want)
	}
}

// A repository URL has no directory on this machine, so the launcher's own —
// where its prompt draft is kept — is the working directory's repository root.
func TestLocalProjectDirectoryForARemoteSourceIsTheWorkingDirectory(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	dir, err := sandboxcreate.LocalProjectDirectory(context.Background(), "https://github.com/discobox-ai/discobox.git@main")
	if err != nil {
		t.Fatalf("LocalProjectDirectory: %v", err)
	}
	if dir != repo {
		t.Fatalf("local project directory = %q, want working directory repo root %q", dir, repo)
	}
}

// ls sends two origin keys (ADR 0111): the one a discobox cut from -C on this
// machine is filed under, and the machine's own, which files its discoboxes
// with no source.
func TestListCommandFiltersSandboxesByOriginKeys(t *testing.T) {
	const host = "host_0123456789abcdef"
	t.Setenv(hostid.EnvVar, host)
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	got := listedOriginKeys(t)
	want := []string{originkey.Of(host, repo), originkey.Host(host)}
	if !slices.Equal(got, want) {
		t.Fatalf("originKey query = %q, want %q", got, want)
	}
}

// With -C naming a repository URL, ls lists what this machine cut from that
// URL, wherever it is run from — not what was started in the directory it ran
// in.
func TestListCommandForARemoteSourceFiltersByItsURL(t *testing.T) {
	const host = "host_0123456789abcdef"
	t.Setenv(hostid.EnvVar, host)
	t.Chdir(newRunSourceTestRepo(t))

	got := listedOriginKeys(t, "-C", "https://github.com/acme/api")
	want := []string{originkey.Of(host, "https://github.com/acme/api"), originkey.Host(host)}
	if !slices.Equal(got, want) {
		t.Fatalf("originKey query = %q, want %q", got, want)
	}
}

// listedOriginKeys runs ls, with args in front of it, against a server that
// records the origin keys the listing asked for.
func listedOriginKeys(t *testing.T, args ...string) []string {
	t.Helper()
	var got []string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got = r.URL.Query()["originKey"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	defer server.Close()

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs(append(append([]string{"--server", server.URL, "--project", "project-1"}, args...), "ls"))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ls: %v", err)
	}
	return got
}
