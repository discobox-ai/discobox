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
)

// liveFixture is a developer's repository served as a live origin: HEAD on
// main, a branch the source declares, and what the allow-list must keep back —
// another branch and a stash.
type liveFixture struct {
	t        *testing.T
	worktree string
	url      string
	// hidden is a commit only the undeclared branch reaches.
	hidden string
	// stash is the commit refs/stash names.
	stash string
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: the pool agent serves git only on Linux")
	}
	worktree := t.TempDir()
	f := &liveFixture{t: t, worktree: worktree}
	f.git("init", "-q", "-b", "main")
	f.commit("README.md", "one\n")
	f.git("branch", "declared")
	f.git("checkout", "-q", "-b", "private")
	f.commit("secret.txt", "credential\n")
	f.hidden = f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git("-c", "user.name=Test", "-c", "user.email=test@example.com", "stash", "-q")
	f.stash = f.git("rev-parse", "refs/stash")

	repo := Repository{Path: filepath.Join(worktree, ".git"), UID: -1, GID: -1, Live: true, Refs: []string{"refs/heads/declared"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, suffix, ok := ParseRepositoryPath(strings.TrimPrefix(r.URL.Path, "/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		ServeBackend(w, r, repo, suffix)
	}))
	t.Cleanup(server.Close)
	f.url = server.URL + "/primary.git"
	return f
}

func (f *liveFixture) git(args ...string) string {
	f.t.Helper()
	return runGit(f.t, f.worktree, args...)
}

func (f *liveFixture) commit(name, content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.worktree, name), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.git("add", name)
	f.git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", name)
	return f.git("rev-parse", "HEAD")
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

// advertisedRefs is what ls-remote lists, asking in the protocol given.
func (f *liveFixture) advertisedRefs(protocol string) []string {
	f.t.Helper()
	var refs []string
	for _, line := range strings.Split(runGit(f.t, "", "-c", "protocol.version="+protocol, "ls-remote", f.url), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// The allow-list is HEAD, the branch HEAD names, and the source's own refs;
// another branch and the stash are not on it, whichever protocol the client
// asks for.
func TestALiveOriginAdvertisesOnlyItsAllowList(t *testing.T) {
	f := newLiveFixture(t)
	want := []string{"HEAD", "refs/heads/declared", "refs/heads/main"}
	for _, protocol := range []string{"0", "2"} {
		if got := f.advertisedRefs(protocol); !slices.Equal(got, want) {
			t.Fatalf("protocol v%s advertised %v, want %v", protocol, got, want)
		}
	}
}

// Hiding a ref means nothing if its commits can be asked for by id. A v2
// upload-pack serves any object it is asked for, so this is what the live
// origin's v0 is for.
func TestALiveOriginRefusesAnObjectNoAdvertisedRefReaches(t *testing.T) {
	f := newLiveFixture(t)
	for _, protocol := range []string{"0", "2"} {
		for name, id := range map[string]string{"a hidden branch's commit": f.hidden, "the stash": f.stash} {
			client := t.TempDir()
			runGit(t, client, "init", "-q")
			if !gitFails(t, client, "-c", "protocol.version="+protocol, "fetch", f.url, id) {
				t.Fatalf("protocol v%s fetched %s by id", protocol, name)
			}
		}
	}
}

// A developer's own configuration cannot widen what the sandbox is served: the
// pool's switches are read after the repository's.
func TestALiveOriginsOwnConfigCannotRevealARef(t *testing.T) {
	f := newLiveFixture(t)
	f.git("config", "uploadpack.allowAnySHA1InWant", "true")
	f.git("config", "--add", "uploadpack.hideRefs", "!refs/heads/private")
	f.git("config", "--add", "transfer.hideRefs", "!refs/stash")
	if got := f.advertisedRefs("2"); slices.Contains(got, "refs/heads/private") || slices.Contains(got, "refs/stash") {
		t.Fatalf("the repository's own config revealed a ref: %v", got)
	}
	client := t.TempDir()
	runGit(t, client, "init", "-q")
	if !gitFails(t, client, "fetch", f.url, f.hidden) {
		t.Fatal("the repository's own config allowed a fetch by id")
	}
}

// The live origin is the developer's repository, so what they commit is there
// for the sandbox's next fetch without anyone pushing — the property ADR 0026
// exists for.
func TestALiveOriginServesANewCommitImmediately(t *testing.T) {
	f := newLiveFixture(t)
	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "clone", "-q", f.url, client)
	added := f.commit("NEW.md", "two\n")
	runGit(t, client, "fetch", "-q", "origin")
	if got := runGit(t, client, "rev-parse", "origin/main"); got != added {
		t.Fatalf("origin/main = %s after the developer committed %s", got, added)
	}
}

// HEAD is read per request: a developer who switches branches moves what the
// sandbox sees as origin's HEAD, and that branch is advertised with it. That
// it can reveal a branch is the price of HEAD being on the list at all, and
// the ADR's: it is where the developer is now.
func TestALiveOriginFollowsTheDevelopersHEAD(t *testing.T) {
	f := newLiveFixture(t)
	f.git("checkout", "-q", "private")
	// main was only ever advertised as HEAD's branch, so it goes with it.
	want := []string{"HEAD", "refs/heads/declared", "refs/heads/private"}
	if got := f.advertisedRefs("2"); !slices.Equal(got, want) {
		t.Fatalf("after switching to private the origin advertised %v, want %v", got, want)
	}
}

// Nothing pushes into the developer's repository, and nothing reads it as
// plain files: the dumb protocol hands out objects whatever is advertised.
func TestALiveOriginIsFetchOnly(t *testing.T) {
	f := newLiveFixture(t)
	before := f.git("for-each-ref")

	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "clone", "-q", f.url, client)
	if err := os.WriteFile(filepath.Join(client, "PUSHED.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, client, "add", "PUSHED.md")
	runGit(t, client, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "pushed")
	if !gitFails(t, client, "push", "origin", "HEAD:refs/heads/pushed") {
		t.Fatal("a push into the live origin succeeded")
	}
	if after := f.git("for-each-ref"); after != before {
		t.Fatalf("the developer's refs changed:\n%s\nwant\n%s", after, before)
	}

	base := strings.TrimSuffix(f.url, "/primary.git")
	for _, path := range []string{
		"/primary.git/info/refs?service=git-receive-pack",
		"/primary.git/info/refs",
		"/primary.git/HEAD",
		"/primary.git/objects/info/packs",
		"/primary.git/objects/" + f.hidden[:2] + "/" + f.hidden[2:],
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
