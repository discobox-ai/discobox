package githttp

import (
	"io"
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

// hideRefs reveals by prefix, so a declared ref that no longer exists would
// reveal whatever the developer later creates beneath its name: deleting the
// declared branch and making declared/x, or leaving HEAD on an unborn branch
// with refs under it. Only refs that exist are revealed, and an existing ref
// has nothing beneath it.
func TestALiveOriginRevealsNothingBeneathARefThatIsGone(t *testing.T) {
	f := newLiveFixture(t)
	f.git("branch", "-D", "declared")
	f.git("branch", "declared/private", "private")
	f.git("symbolic-ref", "HEAD", "refs/heads/unborn")
	f.git("branch", "unborn/private", "private")

	if got, want := f.advertisedRefs("2"), []string(nil); !slices.Equal(got, want) {
		t.Fatalf("with every allowed ref gone the origin advertised %v, want nothing", got)
	}
	client := t.TempDir()
	runGit(t, client, "init", "-q")
	if !gitFails(t, client, "fetch", f.url, f.hidden) {
		t.Fatal("a commit only refs beneath a vanished allowed ref reach was fetched by id")
	}
}

// A name that is a namespace rather than a ref would reveal everything in it.
func TestAnAdvertisedRefMustBeAFullRefName(t *testing.T) {
	for _, ref := range []string{"refs/heads", "refs/heads/", "refs/", "refs//x", "heads/main", "refs/heads/a..b", "refs/heads/*"} {
		if validAdvertisedRef(ref) {
			t.Errorf("validAdvertisedRef(%q) = true", ref)
		}
	}
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/discobox/run/run-1"} {
		if !validAdvertisedRef(ref) {
			t.Errorf("validAdvertisedRef(%q) = false", ref)
		}
	}
}

// http-backend reads the last service parameter and Go the first, so a
// request naming both is a push wherever it is judged.
func TestARequestNamingBothServicesIsAPush(t *testing.T) {
	f := newLiveFixture(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+"/info/refs?service=git-upload-pack&service=git-receive-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsReceivePack(req) {
		t.Fatal("a request naming receive-pack second was not taken for a push")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("live origin answered %d, want 403", resp.StatusCode)
	}
}

// What a request serves is fixed when its snapshot is taken: a developer who
// deletes an allowed branch and creates a private one beneath its name before
// the backend reads anything — the gap a check-then-hideRefs design leaves —
// changes nothing the backend can see.
func TestALiveOriginSnapshotIsFixedBeforeTheBackendReadsIt(t *testing.T) {
	f := newLiveFixture(t)
	declared := f.git("rev-parse", "refs/heads/declared")
	repo := Repository{Path: filepath.Join(f.worktree, ".git"), UID: -1, GID: -1, Live: true, Refs: []string{"refs/heads/declared"}}
	snapshot, err := liveSnapshot(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(snapshot) })

	f.git("branch", "-D", "declared")
	f.git("branch", "declared/private", "private")

	refs := strings.Split(runGit(t, "", "ls-remote", snapshot), "\n")
	want := []string{
		f.git("rev-parse", "main") + "\tHEAD",
		declared + "\trefs/heads/declared",
		f.git("rev-parse", "main") + "\trefs/heads/main",
	}
	if !slices.Equal(refs, want) {
		t.Fatalf("the snapshot serves %q, want %q", refs, want)
	}
}

// A detached HEAD is served as the commit it is, and only that.
func TestALiveOriginServesADetachedHEAD(t *testing.T) {
	f := newLiveFixture(t)
	f.git("checkout", "-q", "--detach", "main")
	if got, want := f.advertisedRefs("2"), []string{"HEAD", "refs/heads/declared"}; !slices.Equal(got, want) {
		t.Fatalf("with HEAD detached the origin advertised %v, want %v", got, want)
	}
}

// Every snapshot is removed once its request is answered.
func TestALiveOriginLeavesNoSnapshotBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := newLiveFixture(t)
	f.advertisedRefs("0")
	runGit(t, "", "clone", "-q", f.url, filepath.Join(t.TempDir(), "clone"))
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("snapshots left behind: %v", left)
	}
}

// serveLive serves gitDir as a live origin with the given declared refs.
func serveLive(t *testing.T, gitDir string, refs []string) string {
	t.Helper()
	repo := Repository{Path: gitDir, UID: -1, GID: -1, Live: true, Refs: refs}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, suffix, ok := ParseRepositoryPath(strings.TrimPrefix(r.URL.Path, "/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		ServeBackend(w, r, repo, suffix)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/primary.git"
}

// A developer's repository that is itself a shallow clone is served with its
// history cut where theirs is.
func TestALiveOriginServesAShallowRepository(t *testing.T) {
	f := newLiveFixture(t)
	f.commit("TWO.md", "two\n")
	shallow := filepath.Join(t.TempDir(), "shallow")
	runGit(t, "", "clone", "-q", "--depth=1", "--no-local", "file://"+f.worktree, shallow)
	url := serveLive(t, filepath.Join(shallow, ".git"), nil)

	client := filepath.Join(t.TempDir(), "client")
	runGit(t, "", "clone", "-q", url, client)
	if got, want := runGit(t, client, "rev-parse", "HEAD"), runGit(t, shallow, "rev-parse", "HEAD"); got != want {
		t.Fatalf("cloned HEAD %s, want %s", got, want)
	}
}

// The shallow file is the one read the pool makes as root rather than as the
// developer, and upload-pack echoes a bad line back to the client. So a
// shallow that leads out of the Git directory, or holds anything but object
// ids, fails the request without its contents reaching the client.
func TestALiveOriginsShallowFileCannotCarryAnotherFileOut(t *testing.T) {
	const secret = "pool-identity-key-material"
	outside := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(outside, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(path string) error{
		"a link out of the Git directory": func(path string) error { return os.Symlink(outside, path) },
		"a file that is not object ids":   func(path string) error { return os.WriteFile(path, []byte(secret+"\n"), 0o600) },
	} {
		f := newLiveFixture(t)
		gitDir := filepath.Join(f.worktree, ".git")
		if err := plant(filepath.Join(gitDir, "shallow")); err != nil {
			t.Fatal(err)
		}
		url := serveLive(t, gitDir, nil)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/info/refs?service=git-upload-pack", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || strings.Contains(string(body), secret) {
			t.Fatalf("%s: status %d, body %q", name, resp.StatusCode, body)
		}
	}
}
