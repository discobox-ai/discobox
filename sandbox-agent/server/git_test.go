package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// gitRoute is a sandbox agent serving one source, primary, checked out at
// worktree, behind a real HTTP server.
type gitRoute struct {
	worktree string
	server   *httptest.Server
	sign     func(projectID, sandboxID, workerID string, scopes ...string) string
}

func newGitRoute(t *testing.T) *gitRoute {
	t.Helper()
	// git on Windows applies its own line-ending translation, and a sandbox
	// agent serves its repositories only where git runs as the sandbox does.
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host")
	}
	worktree := filepath.Join(t.TempDir(), "primary")
	runGit(t, "", "init", "-q", "-b", "main", worktree)
	writeAndCommit(t, worktree, "one\n", "one")

	publicKey, sign := sandboxAgentTestSigner(t)
	cfg := testConfig(publicKey)
	cfg.Sources = []sandboxconfig.Source{{Slug: "primary", Target: worktree}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &gitRoute{worktree: worktree, server: server, sign: sign}
}

func (g *gitRoute) url(sandboxID, slug string) string {
	return g.server.URL + "/api/projects/project-1/sandboxes/" + sandboxID + "/git-repositories/" + slug + ".git"
}

func (g *gitRoute) token(scopes ...string) string {
	return g.sign("project-1", "sandbox-1", "worker-1", scopes...)
}

func bearer(token string) string { return "http.extraHeader=Authorization: Bearer " + token }

// The sandbox serves its own checkout: a clone sees it, and a push with
// sandbox:write lands in the worktree itself, checked out (updateInstead) —
// the way work has always reached a sandbox over this route.
func TestTheGitRouteServesTheSandboxsOwnCheckout(t *testing.T) {
	g := newGitRoute(t)
	write := g.token(ScopeSandboxRead, ScopeSandboxWrite)

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "-c", bearer(write), "clone", "-q", g.url("sandbox-1", "primary"), clone)
	if got := runGit(t, clone, "rev-parse", "HEAD"); got != runGit(t, g.worktree, "rev-parse", "HEAD") {
		t.Fatalf("clone HEAD = %s, want the sandbox's", got)
	}
	pushed := writeAndCommit(t, clone, "two\n", "two")
	runGit(t, clone, "-c", bearer(write), "push", "-q", "origin", "main")

	if got := runGit(t, g.worktree, "rev-parse", "HEAD"); got != pushed {
		t.Fatalf("sandbox HEAD = %s, want the pushed %s", got, pushed)
	}
	data, err := os.ReadFile(filepath.Join(g.worktree, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "two\n" {
		t.Fatalf("sandbox worktree README = %q, want the pushed content", data)
	}

	// And a commit made in the sandbox is what the next fetch brings back,
	// which is all `discobox apply` asks of the route.
	made := writeAndCommit(t, g.worktree, "three\n", "three")
	runGit(t, clone, "-c", bearer(g.token(ScopeSandboxRead)), "fetch", "-q", "origin")
	if got := runGit(t, clone, "rev-parse", "origin/main"); got != made {
		t.Fatalf("fetched origin/main = %s, want the sandbox's %s", got, made)
	}
}

// sandbox:read fetches and pushes nothing; neither does a token for any other
// authority, nor one for another sandbox.
func TestTheGitRouteKeepsReadAndWriteApart(t *testing.T) {
	g := newGitRoute(t)
	read := g.token(ScopeSandboxRead)

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "-c", bearer(read), "clone", "-q", g.url("sandbox-1", "primary"), clone)
	before := runGit(t, g.worktree, "rev-parse", "HEAD")
	writeAndCommit(t, clone, "two\n", "two")
	if err := gitCommand(t, clone, "-c", bearer(read), "push", "-q", "origin", "main").Run(); err == nil {
		t.Fatal("a sandbox:read token pushed")
	}
	if after := runGit(t, g.worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("sandbox HEAD moved to %s on a refused push", after)
	}

	for name, token := range map[string]string{
		"an exec token":            g.token(ScopeExecRead, ScopeExecWrite),
		"another sandbox's token":  g.sign("project-1", "sandbox-2", "worker-1", ScopeSandboxRead, ScopeSandboxWrite),
		"a pool-only scoped token": g.token(ScopeRuntimeConfig),
	} {
		if status := g.status(t, http.MethodGet, g.url("sandbox-1", "primary")+"/info/refs?service=git-upload-pack", token); status != http.StatusForbidden {
			t.Errorf("fetch with %s = %d, want 403", name, status)
		}
	}
	if status := g.status(t, http.MethodGet, g.url("sandbox-1", "primary")+"/info/refs?service=git-upload-pack", ""); status != http.StatusUnauthorized {
		t.Errorf("fetch with no token = %d, want 401", status)
	}
}

// A repository is named only among this sandbox's sources: another sandbox's
// id in the path is refused, and a slug that is no source here is not found,
// whatever lies on disk.
func TestTheGitRouteNamesOnlyThisSandboxsSources(t *testing.T) {
	g := newGitRoute(t)
	read := g.token(ScopeSandboxRead)

	if status := g.status(t, http.MethodGet, g.url("sandbox-2", "primary")+"/info/refs?service=git-upload-pack", g.sign("project-1", "sandbox-2", "worker-1", ScopeSandboxRead)); status != http.StatusForbidden {
		t.Errorf("another sandbox's repository = %d, want 403", status)
	}
	if status := g.status(t, http.MethodGet, g.url("sandbox-1", "other")+"/info/refs?service=git-upload-pack", read); status != http.StatusNotFound {
		t.Errorf("a slug that is no source = %d, want 404", status)
	}
	if status := g.status(t, http.MethodGet, g.server.URL+"/api/projects/project-1/sandboxes/sandbox-1/git-repositories/..%2F..%2Fetc.git/info/refs", read); status != http.StatusNotFound {
		t.Errorf("a slug reaching out of the sources = %d, want 404", status)
	}
}

// The router matches the escaped path, so that is what the token is checked
// against: an escaped slash in the sandbox id cannot make the scope check see
// some other route while the router serves the Git one, or make the identity
// check see this sandbox while the route names another.
func TestTheGitRouteCannotBeReachedThroughAnEscapedPath(t *testing.T) {
	g := newGitRoute(t)
	narrow := g.token(ScopeStatusRead, ScopeTCPConnect)
	for _, path := range []string{
		"/api/projects/project-1/sandboxes/sandbox-1%2Fx/git-repositories/primary.git/info/refs?service=git-upload-pack",
		"/api/projects/project-1/sandboxes/sandbox-1%2Fexecs/git-repositories/primary.git/info/refs?service=git-upload-pack",
	} {
		for name, token := range map[string]string{"a narrow token": narrow, "a sandbox:read token": g.token(ScopeSandboxRead)} {
			if status := g.status(t, http.MethodGet, g.server.URL+path, token); status != http.StatusForbidden {
				t.Errorf("GET %s with %s = %d, want 403", path, name, status)
			}
		}
	}
}

// A source whose checkout has not arrived yet — a push-delivered one before
// its delivery lands — is not found, rather than served from whatever the
// directory holds.
func TestTheGitRouteDoesNotServeACheckoutThatHasNotArrived(t *testing.T) {
	publicKey, sign := sandboxAgentTestSigner(t)
	cfg := testConfig(publicKey)
	cfg.Sources = []sandboxconfig.Source{{Slug: "primary", Target: t.TempDir(), AwaitsDelivery: true}}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+sign("project-1", "sandbox-1", "worker-1", ScopeSandboxRead))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("an undelivered checkout = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
}

// The Git route is gated by its route segment, not by a suffix test further
// down: a source slugged like another route's last segment is still a
// repository.
func TestTheGitRoutesScopeIsNotTakenFromItsSlug(t *testing.T) {
	for _, tc := range []struct {
		method, path, want string
	}{
		{http.MethodGet, "/execs.git/info/refs?service=git-upload-pack", ScopeSandboxRead},
		{http.MethodPost, "/execs.git/git-receive-pack", ScopeSandboxWrite},
		{http.MethodGet, "/status.git/info/refs?service=git-upload-pack", ScopeSandboxRead},
		{http.MethodPost, "/judge.git/git-upload-pack", ScopeSandboxRead},
		{http.MethodGet, "/meta.git/info/refs?service=git-upload-pack&service=git-receive-pack", ScopeSandboxWrite},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/projects/project-1/sandboxes/sandbox-1/git-repositories"+tc.path, nil)
		if got := requiredRequestScope(req); got != tc.want {
			t.Errorf("%s %s requires %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
	// And an exec whose id is git-repositories is still an exec.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs/git-repositories/attach", nil)
	if got := requiredRequestScope(req); got != ScopeExecWrite {
		t.Errorf("an exec named git-repositories requires %q, want %q", got, ScopeExecWrite)
	}
}

func (g *gitRoute) status(t *testing.T, method, target, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func writeAndCommit(t *testing.T, dir, readme, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", message)
	return runGit(t, dir, "rev-parse", "HEAD")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCommand(t, dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd
}
