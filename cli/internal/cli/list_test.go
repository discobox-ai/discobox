package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/internal/hostid"
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

// ls lists every discobox in the project, whatever it was cut from: no origin
// key is sent, and the directory it runs in does not narrow it (ADR
// 26-10-10-954). The commands that take a discobox by name
// list the same way, so a name is matched against everything ls shows.
func TestListingsAreNotScopedToADirectory(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(newRunSourceTestRepo(t))

	for _, args := range [][]string{{"ls"}, {"ls", "--tag", "wip"}, {"rm", "no-such-name"}, {"stop", "no-such-name"}, {"tag", "no-such-name", "wip"}, {"rename", "no-such-name", "other"}} {
		if got := listedOriginKeys(t, args...); len(got) != 0 {
			t.Fatalf("%v: originKey query = %q, want none", args, got)
		}
	}
}

// --source is the SOURCE column's text matched exactly: a directory, a URL, or
// "-" for the discoboxes with no source. A prefix, a trailing slash, or the
// URL without its .git is another string and matches nothing.
func TestListCommandSourceMatchesTheSourceColumnExactly(t *testing.T) {
	row := func(id, source string) string {
		return fmt.Sprintf(`{"id":%q,"projectId":"project-1","createdByUserId":"user-1","displayName":%q,`+
			`"config":{"name":%q,"image":""%s},"runtime":%s,`+
			`"createdAt":"2026-08-19T00:00:00Z","updatedAt":"2026-08-19T00:00:01Z"}`, id, id, id, source, runningRuntime)
	}
	listing := `{"sandboxes":[` + strings.Join([]string{
		row("sbx_dir", `,"source":{"kind":"git","localDirectory":"/home/me/src/api"}`),
		row("sbx_url", `,"source":{"kind":"git","url":"https://github.com/acme/api.git"}`),
		row("sbx_none", ""),
	}, ",") + `]}`
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if keys := r.URL.Query()["originKey"]; len(keys) != 0 {
			t.Errorf("originKey query = %q, want none", keys)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listing))
	}))
	defer server.Close()

	for source, want := range map[string]string{
		"/home/me/src/api":                "sbx_dir",
		"https://github.com/acme/api.git": "sbx_url",
		"-":                               "sbx_none",
		"/home/me/src/api/":               "",
		"/home/me/src":                    "",
		"https://github.com/acme/api":     "",
	} {
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "ls", "-q", "--source", source})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ls --source %s: %v", source, err)
		}
		if got := strings.TrimSpace(out.String()); got != want {
			t.Fatalf("ls --source %s listed %q, want %q", source, got, want)
		}
	}
}

// A source a discobox wrote is printed escaped, so an escape sequence in it
// is shown rather than obeyed, and --source matches the escaped text the
// column shows.
func TestListCommandEscapesTheSourceColumn(t *testing.T) {
	listing := `{"sandboxes":[{"id":"sbx_esc","projectId":"project-1","createdByUserId":"user-1","displayName":"esc",` +
		`"config":{"name":"esc","image":"","source":{"kind":"git","localDirectory":"/home/me/\u001b[2Jx"}},"runtime":` + runningRuntime + `,` +
		`"createdAt":"2026-08-19T00:00:00Z","updatedAt":"2026-08-19T00:00:01Z"}]}`
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listing))
	}))
	defer server.Close()

	run := func(args ...string) string {
		t.Helper()
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "ls"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ls %v: %v", args, err)
		}
		return out.String()
	}
	table := run()
	if strings.Contains(table, "\x1b") {
		t.Fatalf("ls printed a raw escape:\n%q", table)
	}
	if !strings.Contains(table, `/home/me/\x1b[2Jx`) {
		t.Fatalf("ls = %q, want the escape shown as text", table)
	}
	if got := strings.TrimSpace(run("-q", "--source", `/home/me/\x1b[2Jx`)); got != "sbx_esc" {
		t.Fatalf("ls --source with the printed text listed %q, want sbx_esc", got)
	}
}

// An empty --source is refused rather than read as no filter, which would list
// everything for somebody who asked for one place.
func TestListCommandRefusesAnEmptySource(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", "--project", "project-1", "ls", "--source", ""})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--source") {
		t.Fatalf("ls --source '' error = %v, want it refused", err)
	}
}

// listedOriginKeys runs args, a command and its flags, against a server that
// records the origin keys its listing asked for. The command may fail: only
// what it asked is read.
func listedOriginKeys(t *testing.T, args ...string) []string {
	t.Helper()
	var got []string
	listed := false
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		listed = true
		got = append(got, r.URL.Query()["originKey"]...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	defer server.Close()

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1"}, args...))
	_ = cmd.Execute()
	if !listed {
		t.Fatalf("%v listed nothing", args)
	}
	return got
}
