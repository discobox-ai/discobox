package sandboxapply

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// Tip is asked its question — has the discobox committed anything to this
// source — when there is no local repository to answer it with, so it is worth
// knowing it reads a real repository over a real HTTP transport and not only
// that it parses a string.
func TestTipReadsTheHeadOfARepositoryOverHTTP(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "projects", "prj_one", "sandboxes", "sbx_one", "git-repositories", "primary.git")
	want := serveBareRepository(t, repo)
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)

	source := apimodel.GitSource{Slug: apiclientgen.NewOptString("primary")}
	got, err := Tip(context.Background(), server.URL, "prj_one", "sbx_one", "tok_test", source)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if got != want {
		t.Fatalf("tip = %q, want the repository's HEAD %q", got, want)
	}
}

// git writes to stderr on a remote operation whenever it has something to say,
// and gitutil.Output hands stdout and stderr back combined. Reading the first
// token of that would make "warning:" the discobox's tip, which matches no
// commit — and a source with nothing to apply would be reported as having work
// stranded, the exact failure this whole path exists to stop.
func TestTipIgnoresWhatGitWritesToStderr(t *testing.T) {
	const head = "ba4f196fcd4e730abe2535890b1aba4d8edb8560"
	out := "warning: redirecting to https://example.invalid/primary.git/\n" + head + "\tHEAD\n"

	got, ok := lsRemoteCommit(out, "HEAD")
	if !ok || got != head {
		t.Fatalf("lsRemoteCommit = %q, %v; want %q from under git's chatter", got, ok, head)
	}
}

// A repository the sandbox has not created yet advertises no HEAD. Reporting
// some other line as the tip would be worse than saying so.
func TestTipRefusesOutputWithNoHead(t *testing.T) {
	out := "warning: redirecting to https://example.invalid/primary.git/\n" +
		"ba4f196fcd4e730abe2535890b1aba4d8edb8560\trefs/heads/other\n"

	if got, ok := lsRemoteCommit(out, "HEAD"); ok {
		t.Fatalf("lsRemoteCommit = %q, want no answer when HEAD is not advertised", got)
	}
}

// A 64-character object id is a SHA-256 repository's, and is as much a commit
// as a SHA-1 one.
func TestTipAcceptsBothHashLengths(t *testing.T) {
	for _, id := range []string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 64),
	} {
		if got, ok := lsRemoteCommit(id+"\tHEAD\n", "HEAD"); !ok || got != id {
			t.Fatalf("lsRemoteCommit(%d-char id) = %q, %v; want the id", len(id), got, ok)
		}
	}
	if got, ok := lsRemoteCommit("not-a-commit\tHEAD\n", "HEAD"); ok {
		t.Fatalf("lsRemoteCommit = %q, want nothing for a field that is not an object id", got)
	}
}

// serveBareRepository makes a bare repository with one commit at dir, ready to
// be served over plain HTTP — `git update-server-info` is what lets ls-remote
// read it off a file server — and returns the commit its HEAD names.
func serveBareRepository(t *testing.T, dir string) string {
	t.Helper()
	work := t.TempDir()
	git := func(repo string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(t.TempDir(), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	git(work, "init", "--quiet", "--initial-branch=main", work)
	git(work, "commit", "--quiet", "--allow-empty", "-m", "one")
	git(work, "push", "--quiet", dir, "HEAD:refs/heads/main")
	git(dir, "update-server-info")
	return git(work, "rev-parse", "HEAD")
}
