package githttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/gitbackend"
)

// cacheFixture is one bare repository holding two sandboxes' namespaces, as a
// pool's cache of a remote does: mine, which the fixture serves fetch-only as
// its sandbox reaches it, and another's, holding a commit mine does not.
type cacheFixture struct {
	t    *testing.T
	repo string
	url  string
	// shared is the commit both namespaces hold, other the one only the
	// other sandbox's does.
	shared string
	other  string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: the pool agent serves git only on Linux")
	}
	f := &cacheFixture{t: t, repo: filepath.Join(t.TempDir(), "cache.git")}
	runGit(t, "", "init", "-q", "--bare", f.repo)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	f.shared = commit(t, work, "README.md", "one\n")
	f.other = commit(t, work, "secret.txt", "credential\n")
	push := func(namespace, refspec string) {
		cmd := gitCommand(t, work, "push", "-q", f.repo, refspec)
		cmd.Env = append(cmd.Env, "GIT_NAMESPACE="+namespace)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("push: %v: %s", err, out)
		}
	}
	push("sbx-mine-primary", f.shared+":refs/heads/main")
	push("sbx-other-primary", f.other+":refs/heads/main")
	runGit(t, f.repo, "update-ref", "refs/cache/heads/main", f.other)
	runGit(t, f.repo, "symbolic-ref", "refs/namespaces/sbx-mine-primary/HEAD", "refs/namespaces/sbx-mine-primary/refs/heads/main")

	f.url = f.serve(Repository{Path: f.repo, UID: -1, GID: -1, Namespace: "sbx-mine-primary", FetchOnly: true})
	return f
}

func (f *cacheFixture) serve(repo Repository) string {
	f.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, suffix, ok := gitbackend.ParseRepositoryPath(strings.TrimPrefix(r.URL.Path, "/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		ServeBackend(w, r, repo, suffix)
	}))
	f.t.Cleanup(server.Close)
	return server.URL + "/primary.git"
}

func commit(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", name)
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

func gitFails(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	return gitCommand(t, dir, args...).Run() != nil
}

func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// advertisedRefs is what ls-remote lists at url, asking in the protocol given.
func advertisedRefs(t *testing.T, url, protocol string) []string {
	t.Helper()
	var refs []string
	for _, line := range strings.Split(runGit(t, "", "-c", "protocol.version="+protocol, "ls-remote", url), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// A sandbox sees its own namespace's refs under their own names, and nothing
// of another namespace or of the cache's record of the remote, whichever
// protocol it asks for.
func TestASandboxIsAdvertisedOnlyItsOwnNamespace(t *testing.T) {
	f := newCacheFixture(t)
	for _, protocol := range []string{"0", "2"} {
		if got, want := advertisedRefs(t, f.url, protocol), []string{"HEAD", "refs/heads/main"}; !slices.Equal(got, want) {
			t.Fatalf("protocol %s advertises %v, want %v", protocol, got, want)
		}
	}
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "-q", f.url, clone)
	if got := runGit(t, clone, "rev-parse", "HEAD"); got != f.shared {
		t.Fatalf("cloned %s, want %s", got, f.shared)
	}
}

// The object store is shared, so another sandbox's commit is in it; asking
// for it by id must not be answered, in the protocol a client would pick to
// get it or any other.
func TestASandboxIsRefusedAnObjectItsNamespaceDoesNotReach(t *testing.T) {
	f := newCacheFixture(t)
	for _, protocol := range []string{"0", "1", "2"} {
		client := filepath.Join(t.TempDir(), "client-"+protocol)
		runGit(t, "", "init", "-q", client)
		if !gitFails(t, client, "-c", "protocol.version="+protocol, "fetch", f.url, f.other) {
			t.Fatalf("protocol %s served another namespace's commit", protocol)
		}
		if !gitFails(t, client, "cat-file", "-e", f.other) {
			t.Fatalf("protocol %s left another namespace's commit in the client", protocol)
		}
	}
}

// A cache's own configuration is the pool's, but the switches are given on
// the command line all the same: one that allowed a want by id would hand out
// every object the cache holds.
func TestACachesOwnConfigCannotLoosenWhatASandboxIsServed(t *testing.T) {
	f := newCacheFixture(t)
	for _, key := range []string{"uploadpack.allowAnySHA1InWant", "uploadpack.allowReachableSHA1InWant", "uploadpack.allowTipSHA1InWant", "uploadpack.allowRefInWant", "http.getanyfile", "http.receivepack"} {
		runGit(t, f.repo, "config", key, "true")
	}
	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "init", "-q", client)
	if !gitFails(t, client, "fetch", f.url, f.other) {
		t.Fatal("the cache's own configuration let a sandbox fetch another namespace's commit")
	}
}

// A sandbox never writes its namespace, and nothing reads a cache as plain
// files: the dumb protocol hands out objects whatever is advertised.
func TestASandboxsRouteIsFetchOnly(t *testing.T) {
	f := newCacheFixture(t)
	before := runGit(t, f.repo, "for-each-ref")

	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "clone", "-q", f.url, client)
	commit(t, client, "PUSHED.md", "x\n")
	if !gitFails(t, client, "push", "origin", "HEAD:refs/heads/pushed") {
		t.Fatal("a sandbox pushed into its namespace")
	}
	if after := runGit(t, f.repo, "for-each-ref"); after != before {
		t.Fatalf("the cache's refs changed:\n%s\nwant\n%s", after, before)
	}

	base := strings.TrimSuffix(f.url, "/primary.git")
	for _, path := range []string{
		"/primary.git/info/refs?service=git-receive-pack",
		"/primary.git/info/refs?service=git-upload-pack&service=git-receive-pack",
		"/primary.git/info/refs",
		"/primary.git/HEAD",
		"/primary.git/objects/info/packs",
		"/primary.git/objects/" + f.other[:2] + "/" + f.other[2:],
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s = %d, want 403", path, resp.StatusCode)
		}
	}
}

// A project member fetches what the cache remembers of the remote, and none
// of any sandbox's namespace.
func TestTheRemoteViewHidesEveryNamespace(t *testing.T) {
	f := newCacheFixture(t)
	url := f.serve(Repository{Path: f.repo, UID: -1, GID: -1, HideNamespaces: true})
	for _, protocol := range []string{"0", "2"} {
		if got, want := advertisedRefs(t, url, protocol), []string{"refs/cache/heads/main"}; !slices.Equal(got, want) {
			t.Fatalf("protocol %s advertises %v, want %v", protocol, got, want)
		}
	}
	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "init", "-q", client)
	if !gitFails(t, client, "push", url, "HEAD:refs/cache/heads/x") {
		t.Fatal("the remote view took a push")
	}
}

// A member's push lands in the namespace it is served in, under the names the
// client gave.
func TestAPushLandsInItsNamespace(t *testing.T) {
	f := newCacheFixture(t)
	url := f.serve(Repository{Path: f.repo, UID: -1, GID: -1, Namespace: "sbx-new-primary"})
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	tip := commit(t, work, "a.txt", "a\n")
	runGit(t, work, "push", "-q", url, "HEAD:refs/heads/feature")
	if got := runGit(t, f.repo, "rev-parse", "refs/namespaces/sbx-new-primary/refs/heads/feature"); got != tip {
		t.Fatalf("pushed branch = %s, want %s", got, tip)
	}
}
