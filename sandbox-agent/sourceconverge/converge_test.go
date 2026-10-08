package sourceconverge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // serves a test origin through git-http-backend; httpoxy (CVE-2016-5386) is fixed in every Go this builds with
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// TestMain lets the test binary stand in for the agent's git-credential mode,
// which is what git runs when an origin's credential helper names it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "git-credential" {
		if err := RunHelper(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const testToken = "sandbox-token"

// origin is a repository served over smart HTTP the way the pool serves one:
// fetch-only, and only to a request carrying the sandbox's bearer token. A
// request without it is a 401 with no challenge, which is what the pool
// answers.
type origin struct {
	t       *testing.T
	work    string // the developer's checkout, which commits land in
	bare    string // what is served
	url     string
	refused atomic.Int64
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	o := &origin{t: t, work: filepath.Join(root, "work"), bare: filepath.Join(root, "served", "primary.git")}
	gitT(t, root, "init", "--quiet", "--initial-branch=main", o.work)
	gitT(t, root, "init", "--quiet", "--bare", "--initial-branch=main", o.bare)
	gitT(t, o.bare, "config", "http.receivepack", "false")
	backend := gitBackend(t, filepath.Join(root, "served"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			o.refused.Add(1)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":401,"detail":"missing_token"}`))
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	o.url = server.URL + "/primary.git"
	return o
}

// gitBackend serves the repositories under root over smart HTTP.
func gitBackend(t *testing.T, root string) http.Handler {
	t.Helper()
	gitExec := strings.TrimSpace(gitT(t, root, "--exec-path"))
	return &cgi.Handler{
		Path: filepath.Join(gitExec, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
}

// commit makes a commit in the developer's checkout and publishes it.
func (o *origin) commit(file, content string) string {
	o.t.Helper()
	writeFile(o.t, filepath.Join(o.work, file), content)
	gitT(o.t, o.work, "add", "-A")
	gitT(o.t, o.work, "-c", "user.name=dev", "-c", "user.email=dev@example.com", "commit", "--quiet", "-m", "change "+file)
	o.publish("main")
	return strings.TrimSpace(gitT(o.t, o.work, "rev-parse", "HEAD"))
}

func (o *origin) publish(refs ...string) {
	o.t.Helper()
	args := append([]string{"push", "--quiet", "--force", o.bare}, refs...)
	gitT(o.t, o.work, args...)
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// harness is a converger with its credential socket up, the test binary as
// its helper, and a home of its own so no git configuration of the machine
// running the tests reaches it.
type harness struct {
	c      *Converger
	home   string
	socket string
	helper string
}

func newHarness(t *testing.T, manifest ...sandboxconfig.Source) *harness {
	t.Helper()
	home := t.TempDir()
	// A short path: a Unix socket's name is limited to about a hundred bytes.
	socketDir, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{home: home, socket: socket, helper: HelperCommand(executable, socket)}
	h.c = New(Config{
		Manifest:         manifest,
		Env:              map[string]string{"HOME": home, "GIT_CONFIG_NOSYSTEM": "1", "PATH": os.Getenv("PATH")},
		CredentialHelper: h.helper,
		RetryInterval:    20 * time.Millisecond,
		MaxRetryInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = h.c.ServeCredentials(ctx, socket) }()
	waitFor(t, "the credential socket", func() bool {
		_, err := os.Stat(socket)
		return err == nil
	})
	return h
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pass converges doc once, synchronously, and returns the source's state.
func (h *harness) pass(t *testing.T, doc sandboxconfig.RuntimeConfig, slug string) SourceState {
	t.Helper()
	h.c.Converge(doc)
	<-h.c.wake
	h.c.pass(context.Background())
	return h.state(t, slug)
}

func (h *harness) state(t *testing.T, slug string) SourceState {
	t.Helper()
	for _, state := range h.c.States() {
		if state.Slug == slug {
			return state
		}
	}
	t.Fatalf("no state reported for %q in %+v", slug, h.c.States())
	return SourceState{}
}

// git runs git in the sandbox's checkout as a user there would: with the
// harness's home and nothing in the environment that knows a token.
func (h *harness) git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + h.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func document(revision int64, sources ...sandboxconfig.RuntimeSource) sandboxconfig.RuntimeConfig {
	return sandboxconfig.RuntimeConfig{Revision: revision, Sources: sources}
}

func TestFreshCloneMaterializesTheSourceOnce(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}

	state := h.pass(t, document(1, source), "primary")
	if state.State != StateMaterialized || state.Commit != first || state.Revision != 1 || state.UpdatedAt.IsZero() {
		t.Fatalf("state = %+v, want materialized at %s under revision 1", state, first)
	}
	if got, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || string(got) != "hello\n" {
		t.Fatalf("README.md = %q, %v", got, err)
	}
	if branch, _ := h.git(t, target, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(branch) != "main" {
		t.Fatalf("checked out %q, want branch main", branch)
	}
	if remote, _ := h.git(t, target, "remote", "get-url", "origin"); strings.TrimSpace(remote) != o.url {
		t.Fatalf("origin = %q, want %q", remote, o.url)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(target, scratchPrefix+"*")); len(leftovers) != 0 {
		t.Fatalf("the scratch clone was left behind: %v", leftovers)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", materializingMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the in-progress marker was left behind: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(target, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), testToken) {
		t.Fatalf("the origin token was written into the checkout:\n%s", config)
	}
	if o.refused.Load() == 0 {
		t.Fatal("the origin was never asked without a token, so the helper was never exercised")
	}

	// Work done in the sandbox survives every later pass: a resume, a
	// re-delivery, a newer revision.
	writeFile(t, filepath.Join(target, "README.md"), "edited in the sandbox\n")
	writeFile(t, filepath.Join(target, "scratch.txt"), "untracked\n")
	o.commit("later.txt", "a commit the sandbox has not fetched\n")
	state = h.pass(t, document(2, source), "primary")
	if state.State != StateMaterialized || state.Commit != first {
		t.Fatalf("state after a resume = %+v, want still materialized at %s", state, first)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "README.md")); string(got) != "edited in the sandbox\n" {
		t.Fatalf("a later pass re-materialized over the sandbox's work: README.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "scratch.txt")); err != nil {
		t.Fatalf("a later pass removed an untracked file: %v", err)
	}
}

func TestFetchSeesANewDeveloperCommit(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	second := o.commit("next.txt", "the developer kept working\n")
	// A user's own fetch, with nothing but the checkout's configuration: the
	// helper hands git the token the agent holds.
	if out, err := h.git(t, target, "fetch", "origin"); err != nil {
		t.Fatalf("git fetch origin: %v\n%s", err, out)
	}
	if got, _ := h.git(t, target, "rev-parse", "origin/main"); strings.TrimSpace(got) != second {
		t.Fatalf("origin/main = %q, want the developer's new commit %s", got, second)
	}
}

func TestAUserCredentialStoreIsNeverHandedTheToken(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t)
	store := filepath.Join(h.home, ".git-credentials")
	writeFile(t, filepath.Join(h.home, ".gitconfig"), "[credential]\n\thelper = store --file "+store+"\n")
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	if out, err := h.git(t, target, "fetch", "origin"); err != nil {
		t.Fatalf("git fetch origin: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(store); err == nil && strings.Contains(string(data), testToken) {
		t.Fatalf("the user's credential store kept the origin token:\n%s", data)
	}
}

func TestATokenTheAgentDoesNotHoldIsNotSent(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t)
	state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, Commit: first}), "primary")
	if state.State != StateFailed || state.Error == "" {
		t.Fatalf("state = %+v, want failed: the origin refuses a request without its token", state)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed clone left a .git at the target: %v", err)
	}
	// The next document carries the token, and the same source converges.
	state = h.pass(t, document(2, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary")
	if state.State != StateMaterialized || state.Error != "" {
		t.Fatalf("state = %+v, want materialized once the token arrives", state)
	}
}

func TestAnEmptyOriginWaitsAndClonesOnceThePushLands(t *testing.T) {
	o := newOrigin(t)
	target := t.TempDir()
	h := newHarness(t)
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken}
	if state := h.pass(t, document(1, source), "primary"); state.State != StateWaiting {
		t.Fatalf("state = %+v, want waiting on an origin with nothing in it", state)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("waiting left %d entries at the target", len(entries))
	}
	pushed := o.commit("README.md", "pushed\n")
	// No new document: the loop retries a waiting source by itself.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.Run(ctx)
	h.c.Converge(document(1, source))
	waitFor(t, "the pushed source to materialize", func() bool { return h.state(t, "primary").State == StateMaterialized })
	if got := h.state(t, "primary").Commit; got != pushed {
		t.Fatalf("materialized at %s, want %s", got, pushed)
	}
}

func TestADirtyWorkspaceSnapshotIsRestoredUnstaged(t *testing.T) {
	o := newOrigin(t)
	base := o.commit("README.md", "hello\n")
	// The client's uncommitted work, as a commit on the snapshot ref whose
	// parent is the base.
	writeFile(t, filepath.Join(o.work, "README.md"), "hello, edited\n")
	writeFile(t, filepath.Join(o.work, "new.txt"), "not yet added\n")
	gitT(t, o.work, "add", "-A")
	tree := strings.TrimSpace(gitT(t, o.work, "write-tree"))
	snapshot := strings.TrimSpace(gitT(t, o.work, "-c", "user.name=dev", "-c", "user.email=dev@example.com", "commit-tree", tree, "-p", base, "-m", "snapshot"))
	gitT(t, o.work, "update-ref", "refs/discobox/workspace", snapshot)
	gitT(t, o.work, "reset", "--quiet", "--hard", base)
	o.publish("main", "refs/discobox/workspace")

	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{
		Slug: "primary", Target: target, RefName: "main", RefType: "branch",
		Workspace: &sandboxconfig.SourceWorkspace{BaseCommit: base, SnapshotRef: "refs/discobox/workspace"},
	})
	state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: base}), "primary")
	if state.State != StateMaterialized || state.Commit != base {
		t.Fatalf("state = %+v, want materialized at the base %s", state, base)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "README.md")); string(got) != "hello, edited\n" {
		t.Fatalf("README.md = %q, want the client's edit", got)
	}
	status, _ := h.git(t, target, "status", "--porcelain")
	if !strings.Contains(status, " M README.md") || !strings.Contains(status, "?? new.txt") {
		t.Fatalf("status = %q, want the edit unstaged and the new file untracked", status)
	}
}

func TestAnInterruptedMaterializationResumes(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}
	if state := h.pass(t, document(1, source), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	// Back to the moment after the clone moved into place and before it was
	// finished: the in-progress marker, no materialized marker, and a tracked
	// file left mid-way beside one the target held all along.
	if err := os.Remove(filepath.Join(target, ".git", sandboxconfig.SourceMaterializedMarker)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, ".git", materializingMarker), "")
	writeFile(t, filepath.Join(target, "README.md"), "half-written\n")
	writeFile(t, filepath.Join(target, "kept.txt"), "the target's own\n")
	if state := h.pass(t, document(2, source), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v, want the interrupted attempt finished", state)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "README.md")); string(got) != "hello\n" {
		t.Fatalf("README.md = %q, want the source's own", got)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "kept.txt")); string(got) != "the target's own\n" {
		t.Fatalf("a resumed attempt removed a file the target held: %q", got)
	}
}

// A failure after the clone is in place — here a pin the origin does not
// have yet — is retried without losing what the target held, and the retry
// fetches what arrived since.
func TestARetryKeepsWhatTheTargetHeldAndFetchesThePin(t *testing.T) {
	o := newOrigin(t)
	o.commit("README.md", "hello\n")
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "nested", ".keep"), "")
	writeFile(t, filepath.Join(target, "notes.txt"), "here before the clone\n")
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	// The pin is a commit the developer has made and not yet published.
	writeFile(t, filepath.Join(o.work, "later.txt"), "later\n")
	gitT(t, o.work, "add", "-A")
	gitT(t, o.work, "-c", "user.name=dev", "-c", "user.email=dev@example.com", "commit", "--quiet", "-m", "later")
	pin := strings.TrimSpace(gitT(t, o.work, "rev-parse", "HEAD"))
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: pin}
	if state := h.pass(t, document(1, source), "primary"); state.State != StateFailed {
		t.Fatalf("state = %+v, want failed on a pin the origin lacks", state)
	}
	o.publish("main")
	if state := h.pass(t, document(1, source), "primary"); state.State != StateMaterialized || state.Commit != pin {
		t.Fatalf("state = %+v, want materialized at %s once it is published", state, pin)
	}
	for _, kept := range []string{"notes.txt", filepath.Join("nested", ".keep")} {
		if _, err := os.Stat(filepath.Join(target, kept)); err != nil {
			t.Fatalf("the retry removed %s, which the target held: %v", kept, err)
		}
	}
}

func TestAnEmptyOriginNamingABranchWaits(t *testing.T) {
	o := newOrigin(t)
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "feature", RefType: "branch"})
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken}), "primary"); state.State != StateWaiting {
		t.Fatalf("state = %+v, want waiting on an origin the branch has not been pushed to", state)
	}
}

func TestAnOriginWhoseHeadNamesNoBranchClonesTheSourcesRef(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	gitT(t, o.bare, "symbolic-ref", "HEAD", "refs/heads/never-pushed")
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken}), "primary"); state.State != StateMaterialized || state.Commit != first {
		t.Fatalf("state = %+v, want materialized at %s", state, first)
	}
}

func TestTheReportedCommitIsTheOneMaterialized(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}
	h.pass(t, document(1, source), "primary")
	writeFile(t, filepath.Join(target, "work.txt"), "work\n")
	if out, err := h.git(t, target, "add", "-A"); err != nil {
		t.Fatal(out)
	}
	if out, err := h.git(t, target, "-c", "user.name=s", "-c", "user.email=s@example.com", "commit", "--quiet", "-m", "sandbox work"); err != nil {
		t.Fatal(out)
	}
	if state := h.pass(t, document(2, source), "primary"); state.Commit != first {
		t.Fatalf("state = %+v, want the commit materialized, %s, not where the sandbox moved since", state, first)
	}
}

func TestAnOriginWithoutATokenKeepsTheUsersOwnHelper(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	// A remote-URL source: served without a token here, as a public remote is.
	backend := gitBackend(t, filepath.Dir(o.bare))
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(public.Close)
	target := t.TempDir()
	h := newHarness(t)
	url := public.URL + "/primary.git"
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: url, Commit: first}), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	config, err := os.ReadFile(filepath.Join(target, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "credential") {
		t.Fatalf("an origin that takes no token was given the agent's helper:\n%s", config)
	}
}

func TestARepositoryTheSandboxDidNotCloneIsLeftAlone(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t)
	gitT(t, target, "init", "--quiet")
	writeFile(t, filepath.Join(target, "mine.txt"), "someone's work\n")
	state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary")
	if state.State != StateFailed || !strings.Contains(state.Error, "did not clone") {
		t.Fatalf("state = %+v, want refused", state)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "mine.txt")); string(got) != "someone's work\n" {
		t.Fatalf("the repository's work was touched: %q", got)
	}
}

func TestANonEmptyTargetKeepsItsFiles(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	writeFile(t, filepath.Join(target, ".credentials.json"), "restored before the source arrived\n")
	writeFile(t, filepath.Join(target, "README.md"), "stale\n")
	h := newHarness(t)
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	if got, _ := os.ReadFile(filepath.Join(target, ".credentials.json")); string(got) != "restored before the source arrived\n" {
		t.Fatalf("a file already at the target was lost: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "README.md")); string(got) != "hello\n" {
		t.Fatalf("README.md = %q, want the source's", got)
	}
}

func TestStatesFollowTheDocument(t *testing.T) {
	h := newHarness(t)
	if states := h.c.States(); states != nil {
		t.Fatalf("states before any document = %+v", states)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	h.c.Converge(document(3, sandboxconfig.RuntimeSource{Slug: "b"}, sandboxconfig.RuntimeSource{Slug: "a", Target: missing, OriginURL: "http://127.0.0.1:1/a.git"}))
	states := h.c.States()
	if len(states) != 2 || states[0].Slug != "b" || states[1].Slug != "a" || states[0].State != StateWaiting || states[0].Revision != 3 || states[0].UpdatedAt.IsZero() {
		t.Fatalf("states before a pass = %+v, want both waiting under revision 3, in document order", states)
	}
	<-h.c.wake
	if !h.c.pass(context.Background()) {
		t.Fatal("a failed source is not retried")
	}
	if state := h.state(t, "a"); state.State != StateFailed || !strings.Contains(state.Error, "source target") {
		t.Fatalf("state = %+v, want failed on the missing target", state)
	}
	if state := h.state(t, "b"); state.State != StateWaiting {
		t.Fatalf("a source with no origin = %+v, want waiting", state)
	}
	h.c.Converge(document(4, sandboxconfig.RuntimeSource{Slug: "b"}))
	if states := h.c.States(); len(states) != 1 || states[0].Slug != "b" {
		t.Fatalf("states after a document dropped a source = %+v", states)
	}
}

func TestProjectLayer(t *testing.T) {
	o := newOrigin(t)
	writeFile(t, filepath.Join(o.work, ".discobox", "project.json"), `{"runCommand":["make","dev"]}`)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	h := newHarness(t)
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}
	h.c.Converge(document(1, source))
	if _, err := h.c.ProjectLayer(context.Background(), "primary"); !errors.Is(err, ErrNotMaterialized) {
		t.Fatalf("before materializing: %v, want ErrNotMaterialized", err)
	}
	if _, err := h.c.ProjectLayer(context.Background(), "other"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("an unknown slug: %v, want ErrUnknownSource", err)
	}
	if state := h.pass(t, document(1, source), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	layer, err := h.c.ProjectLayer(context.Background(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(layer.Layer, &got); err != nil || layer.Commit != first || layer.Slug != "primary" {
		t.Fatalf("layer = %+v (%v), want the file at %s", layer, err, first)
	}
	if _, ok := got["runCommand"]; !ok {
		t.Fatalf("layer = %s", layer.Layer)
	}

	// The read stays inside the checkout whatever the repository links to.
	outside := filepath.Join(t.TempDir(), "secret.json")
	writeFile(t, outside, `{"secret":true}`)
	if err := os.Remove(filepath.Join(target, ".discobox", "project.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, ".discobox", "project.json")); err != nil {
		t.Fatal(err)
	}
	if layer, err := h.c.ProjectLayer(context.Background(), "primary"); !errors.Is(err, ErrInvalidProjectLayer) {
		t.Fatalf("a link out of the checkout read %s, %v", layer.Layer, err)
	}

	if err := os.Remove(filepath.Join(target, ".discobox", "project.json")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, ".discobox", "project.json"), `["not", "an", "object"]`)
	if _, err := h.c.ProjectLayer(context.Background(), "primary"); !errors.Is(err, ErrInvalidProjectLayer) {
		t.Fatalf("a non-object: %v, want ErrInvalidProjectLayer", err)
	}
	if err := os.RemoveAll(filepath.Join(target, ".discobox")); err != nil {
		t.Fatal(err)
	}
	layer, err = h.c.ProjectLayer(context.Background(), "primary")
	if err != nil || layer.Layer != nil || layer.Commit != first {
		t.Fatalf("no project layer = %+v, %v; want none, at %s", layer, err, first)
	}
}

func TestTokenMatchesTheOriginGitNames(t *testing.T) {
	c := New(Config{})
	c.Converge(document(1,
		sandboxconfig.RuntimeSource{Slug: "a", Target: "/w/a", OriginURL: "https://pool:8443/api/x/git-origins/a.git", OriginToken: "ta"},
		sandboxconfig.RuntimeSource{Slug: "b", Target: "/w/b", OriginURL: "https://pool:8443/api/x/git-origins/b.git", OriginToken: "tb"},
		sandboxconfig.RuntimeSource{Slug: "c", Target: "/w/c", OriginURL: "https://github.com/o/c.git"},
	))
	for _, tc := range []struct{ protocol, host, path, want string }{
		{"https", "pool:8443", "api/x/git-origins/a.git", "ta"},
		{"https", "pool:8443", "api/x/git-origins/b.git", "tb"},
		{"https", "pool:8443", "api/x/git-origins/a.git/", "ta"},
		{"http", "pool:8443", "api/x/git-origins/a.git", ""},
		{"https", "pool", "api/x/git-origins/a.git", ""},
		{"https", "pool:8443", "", ""},
		{"https", "github.com", "o/c.git", ""},
	} {
		if got := c.Token(tc.protocol, tc.host, tc.path); got != tc.want {
			t.Errorf("Token(%q, %q, %q) = %q, want %q", tc.protocol, tc.host, tc.path, got, tc.want)
		}
	}
}

func TestHelperAnswersOnlyAGitThatTakesABearer(t *testing.T) {
	h := newHarness(t)
	h.c.Converge(document(1, sandboxconfig.RuntimeSource{Slug: "a", Target: "/w/a", OriginURL: "https://pool/a.git", OriginToken: "tok"}))
	run := func(op, input string) string {
		var out strings.Builder
		if err := RunHelper([]string{"--socket", h.socket, op}, strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run("get", "protocol=https\nhost=pool\npath=a.git\ncapability[]=authtype\n\n"); !strings.Contains(got, "authtype=Bearer\ncredential=tok\n") || !strings.Contains(got, "ephemeral=1") {
		t.Fatalf("answer = %q", got)
	}
	if got := run("get", "protocol=https\nhost=pool\npath=a.git\n\n"); got != "" {
		t.Fatalf("a git without the authtype capability was answered %q", got)
	}
	if got := run("get", "protocol=https\nhost=pool\npath=other.git\ncapability[]=authtype\n\n"); got != "" {
		t.Fatalf("an origin the agent has no token for was answered %q", got)
	}
	if got := run("store", "protocol=https\nhost=pool\npath=a.git\n\n"); got != "" {
		t.Fatalf("store answered %q", got)
	}
}

// A document that moves the pin while an attempt runs must not let the old
// attempt mark the source materialized at the old commit: marking is once-only,
// so the new pin would never be applied.
func TestASupersededAttemptDoesNotMaterializeTheOldPin(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	second := o.commit("README.md", "re-pinned\n")
	target := t.TempDir()
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	old := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}
	repinned := old
	repinned.Commit = second
	// The re-pin arrives, then the attempt begun under revision 1 completes.
	h.c.Converge(document(2, repinned))
	<-h.c.wake
	if state := h.c.converge(context.Background(), 1, old); state.State != StateCloning {
		t.Fatalf("the superseded attempt = %+v, want it left unfinished", state)
	}
	if _, ok := materializedAt(target); ok {
		t.Fatal("a superseded attempt marked the source materialized at the old pin")
	}
	// The next pass goes on from that checkout to the new pin.
	h.c.pass(context.Background())
	if state := h.state(t, "primary"); state.State != StateMaterialized || state.Commit != second || state.Revision != 2 {
		t.Fatalf("state = %+v, want materialized at the new pin %s", state, second)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "README.md")); string(got) != "re-pinned\n" {
		t.Fatalf("README.md = %q, want the new pin's", got)
	}
}

// Scratch directories are the agent's own: one in the target that merely
// shares the name is the sandbox's, and a materialization leaves it be.
func TestAPathSharingTheScratchNameIsKept(t *testing.T) {
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := t.TempDir()
	theirs := filepath.Join(target, scratchPrefix+"theirs")
	writeFile(t, filepath.Join(theirs, "work.txt"), "the sandbox's\n")
	// And one an earlier attempt of this agent's left behind, which goes.
	ours := filepath.Join(target, scratchPrefix+"stale")
	writeFile(t, filepath.Join(ours, scratchOwnedMarker), "")
	h := newHarness(t)
	if state := h.pass(t, document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}), "primary"); state.State != StateMaterialized {
		t.Fatalf("state = %+v", state)
	}
	if got, _ := os.ReadFile(filepath.Join(theirs, "work.txt")); string(got) != "the sandbox's\n" {
		t.Fatalf("a directory sharing the scratch name was removed: %q", got)
	}
	if _, err := os.Stat(ours); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an earlier attempt's scratch was left: %v", err)
	}
}
