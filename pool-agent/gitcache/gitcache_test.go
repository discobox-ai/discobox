package gitcache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == PreReceiveCommand {
		os.Exit(RunPreReceive(os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}

// work is a throwaway repository a test commits in and pushes from.
type work struct {
	t   *testing.T
	dir string
}

func newWork(t *testing.T) *work {
	t.Helper()
	w := &work{t: t, dir: t.TempDir()}
	w.git("init", "--quiet", "--initial-branch=main")
	return w
}

func (w *work) git(args ...string) string {
	w.t.Helper()
	return w.gitEnv(nil, args...)
}

func (w *work) gitEnv(env []string, args ...string) string {
	w.t.Helper()
	out, err := w.try(env, args...)
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (w *work) try(env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(w.t.Context(), "git", args...)
	cmd.Dir = w.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (w *work) commit(name string) string {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, name), []byte(name+"\n"), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.git("add", name)
	w.git("commit", "--quiet", "-m", name)
	return w.git("rev-parse", "HEAD")
}

// push sends refspecs into a source's namespace of the cache the way the
// pool's backend receives them: under ReceiveConfig, as a member or as the
// sandbox caller.
func (w *work) push(s *Store, path, sandboxID, slug, caller string, refspecs ...string) (string, error) {
	w.t.Helper()
	before, err := s.Receiving(context.Background(), path, sandboxID, slug)
	if err != nil {
		w.t.Fatal(err)
	}
	config := ReceiveConfig()
	env := []string{"GIT_NAMESPACE=" + Namespace(sandboxID, slug)}
	if caller == "" {
		config = append(config, MemberReceiveConfig()...)
	} else {
		extra, hookEnv, err := s.SandboxReceiveConfig(path, caller, sandboxID, slug)
		if err != nil {
			w.t.Fatal(err)
		}
		config = append(config, extra...)
		env = append(env, hookEnv...)
	}
	receive := "git " + strings.Join(config, " ") + " receive-pack"
	out, err := w.try(env, append([]string{"push", "--receive-pack=" + receive, path}, refspecs...)...)
	if err == nil {
		if err := s.Delivered(context.Background(), path, sandboxID, slug, caller == "", before); err != nil {
			w.t.Fatal(err)
		}
	}
	return out, err
}

// nameWithoutSending pushes one branch at object into a source's namespace as
// the sandbox caller, sending no objects at all, and returns what receive-pack
// answered.
func nameWithoutSending(t *testing.T, s *Store, path, caller, sandboxID, slug, object string) (string, error) {
	t.Helper()
	if _, err := s.Receiving(context.Background(), path, sandboxID, slug); err != nil {
		t.Fatal(err)
	}
	config, env, err := s.SandboxReceiveConfig(path, caller, sandboxID, slug)
	if err != nil {
		t.Fatal(err)
	}
	empty := exec.CommandContext(t.Context(), "git", "pack-objects", "--stdout")
	empty.Dir = t.TempDir()
	empty.Stdin = strings.NewReader("")
	//nolint:gosec // a directory this test made
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--quiet", empty.Dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	pack, err := empty.Output()
	if err != nil {
		t.Fatal(err)
	}
	update := strings.Repeat("0", len(object)) + " " + object + " refs/heads/main\x00report-status\n"
	request := fmt.Sprintf("%04x%s0000", len(update)+4, update) + string(pack)
	//nolint:gosec // the switches a cache is served under, and a cache this test made
	cmd := exec.CommandContext(t.Context(), "git", append(append(ReceiveConfig(), config...), "receive-pack", path)...)
	cmd.Env = append(append(os.Environ(), "GIT_NAMESPACE="+Namespace(sandboxID, slug)), env...)
	cmd.Stdin = strings.NewReader(request)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// hasBranches is Store.HasBranches for a cache that reads.
func hasBranches(t *testing.T, s *Store, path, sandboxID, slug string) bool {
	t.Helper()
	held, err := s.HasBranches(context.Background(), path, sandboxID, slug)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func use(t *testing.T, s *Store, key string) string {
	t.Helper()
	path, release, err := s.Use(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	release()
	return path
}

func refs(t *testing.T, path string, pattern string) []string {
	t.Helper()
	out, err := git(context.Background(), path, nil, nil, "for-each-ref", "--format=%(refname)", pattern)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(out)
}

func TestAPinCreatesWhatTheCacheHoldsAndReportsTheRest(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	first := w.commit("a")
	second := w.commit("b")
	if out, err := w.push(s, path, "sbx-one", "widget", "", first+":refs/heads/main", first+":refs/remotes/origin/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// Everything asked for is held: nothing is transferred.
	result, err := s.Pin(ctx, path, "sbx-two", "widget", map[string]string{
		"refs/heads/main":          first,
		"refs/remotes/origin/main": first,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Missing) != 0 || !slices.Equal(result.Pinned, []string{"refs/heads/main", "refs/remotes/origin/main"}) {
		t.Fatalf("pin = %+v", result)
	}
	if got := resolve(ctx, path, "refs/namespaces/sbx-two-widget/refs/heads/main"); got != first {
		t.Fatalf("pinned branch = %q, want %q", got, first)
	}
	if got, _ := git(ctx, path, nil, nil, "symbolic-ref", "refs/namespaces/sbx-two-widget/HEAD"); got != "refs/namespaces/sbx-two-widget/refs/heads/main" {
		t.Fatalf("namespace HEAD = %q", got)
	}

	// One object is not held: the branch waits for the push that brings it,
	// so a sandbox never clones a namespace that is half delivered.
	result, err = s.Pin(ctx, path, "sbx-three", "widget", map[string]string{
		"refs/heads/main":          first,
		"refs/remotes/origin/main": second,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Missing, []string{"refs/heads/main", "refs/remotes/origin/main"}) || len(result.Pinned) != 0 {
		t.Fatalf("pin = %+v", result)
	}
	if hasBranches(t, s, path, "sbx-three", "widget") {
		t.Fatal("a branch was pinned while another ref was missing")
	}
}

func TestAMembersDeliveryRecordsTheRemoteOutsideEveryNamespace(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	tip := w.commit("a")
	w.git("tag", "v1")
	if out, err := w.push(s, path, "sbx-one", "widget", "", tip+":refs/heads/main", tip+":refs/remotes/origin/main", "refs/tags/v1"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := refs(t, path, RemoteRefPrefix); !slices.Equal(got, []string{"refs/cache/heads/main", "refs/cache/tags/v1"}) {
		t.Fatalf("remote refs = %v", got)
	}
}

// The record of a remote is what was last delivered, not everything ever
// delivered: a branch the remote dropped goes, which is also what lets the
// remote replace a branch with branches beneath its name.
func TestTheRecordOfARemoteFollowsWhatWasLastDelivered(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	tip := w.commit("a")
	if out, err := w.push(s, path, "sbx-one", "widget", "", tip+":refs/heads/main", tip+":refs/remotes/origin/main", tip+":refs/remotes/origin/release", tip+":refs/remotes/origin/HEAD"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := w.push(s, path, "sbx-two", "widget", "", tip+":refs/heads/main", tip+":refs/remotes/origin/main", tip+":refs/remotes/origin/release/1.0"); err != nil {
		t.Fatalf("a delivery after the remote renamed a branch beneath itself: %v\n%s", err, out)
	}
	if got := refs(t, path, RemoteRefPrefix); !slices.Equal(got, []string{"refs/cache/heads/main", "refs/cache/heads/release/1.0"}) {
		t.Fatalf("remote refs = %v", got)
	}
	// A later push into an older sandbox's namespace moves its branch and
	// says nothing of the remote: its view of the remote, as old as the
	// sandbox, is not written back over the newer record.
	next := w.commit("b")
	if out, err := w.push(s, path, "sbx-one", "widget", "", next+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := refs(t, path, RemoteRefPrefix); !slices.Equal(got, []string{"refs/cache/heads/main", "refs/cache/heads/release/1.0"}) {
		t.Fatalf("remote refs after a push into an older namespace = %v", got)
	}
	// A delivery that brings only a tag still records the tag.
	w.git("tag", "v2")
	if out, err := w.push(s, path, "sbx-tagged", "widget", "", next+":refs/heads/main", "refs/tags/v2"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := refs(t, path, remoteTags); !slices.Equal(got, []string{"refs/cache/tags/v2"}) {
		t.Fatalf("recorded tags after a tag-only delivery = %v", got)
	}
	// A delivery that names none of the remote's branches says nothing of them.
	if out, err := w.push(s, path, "sbx-three", "widget", "", tip+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := refs(t, path, remoteHeads); len(got) != 2 {
		t.Fatalf("remote refs after a delivery with no remote branches = %v", got)
	}
}

// A sweep is due an interval after the last one began, by a record that
// outlives the pool agent: restarting it neither sweeps at once nor puts the
// sweep off for ever.
func TestASweepIsDueByARecordThatOutlivesTheProcess(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	root := filepath.Join(t.TempDir(), "caches")
	s := New(root)
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	gone := w.commit("gone")
	if out, err := w.push(s, path, "sbx-gone", "widget", "", gone+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	held := func(context.Context) ([]string, error) { return nil, nil }
	opts := SweepOptions{NamespaceRetention: time.Minute, CacheRetention: 24 * time.Hour}
	now := time.Now()
	marker := filepath.Join(path, namespacesDir, "sbx-gone-widget"+unheldSuffix)

	New(root).SweepIfDue(ctx, time.Hour, held, opts, now, logger)
	New(root).SweepIfDue(ctx, time.Hour, held, opts, now.Add(30*time.Minute), logger)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a store swept before an interval had passed")
	}
	// Another process, later: the interval has passed since the record.
	New(root).SweepIfDue(ctx, time.Hour, held, opts, now.Add(2*time.Hour), logger)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a restarted store never swept: %v", err)
	}
}

func TestASandboxDeliversOnlyWhatItSendsAndWhatItsOwnSourcesHold(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")

	// A member's sandbox holds a secret commit; the calling sandbox holds its
	// own history in the same cache.
	secret := newWork(t)
	secretTip := secret.commit("secret")
	if out, err := secret.push(s, path, "sbx-other", "widget", "", secretTip+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	mine := newWork(t)
	base := mine.commit("mine")
	if out, err := mine.push(s, path, "sbx-caller", "widget", "", base+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// It is told its own namespaces' tips, and nobody else's.
	config, _, err := s.SandboxReceiveConfig(path, "sbx-caller", "sbx-child", "widget")
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // the switches a cache is served under, and a cache this test made
	cmd := exec.CommandContext(t.Context(), "git", append(append(ReceiveConfig(), config...), "receive-pack", "--advertise-refs", path)...)
	cmd.Env = append(os.Environ(), "GIT_NAMESPACE=sbx-child-widget")
	advertised, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(advertised), base) || strings.Contains(string(advertised), secretTip) {
		t.Fatalf("advertisement = %q", advertised)
	}

	// A delta on top of what it holds is accepted.
	next := mine.commit("next")
	if out, err := mine.push(s, path, "sbx-child", "widget", "sbx-caller", next+":refs/heads/main"); err != nil {
		t.Fatalf("a sandbox's own delta was refused: %v\n%s", err, out)
	}

	// And so is the next one into the same discobox: the caller is told
	// what that namespace holds, so its push leaves it out.
	after := mine.commit("after")
	if out, err := mine.push(s, path, "sbx-child", "widget", "sbx-caller", after+":refs/heads/main"); err != nil {
		t.Fatalf("a sandbox's second push into a discobox it created was refused: %v\n%s", err, out)
	}

	// A ref at an object it never sent and does not hold is refused, though
	// the store has the object: git alone would accept it. No stock client
	// sends such a push, so it is spoken by hand: one ref update and a pack
	// with nothing in it.
	out, err := nameWithoutSending(t, s, path, "sbx-caller", "sbx-child2", "widget", secretTip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "did not send") {
		t.Fatalf("a ref at another sandbox's commit was accepted:\n%s", out)
	}
	if hasBranches(t, s, path, "sbx-child2", "widget") {
		t.Fatal("the refused ref was created")
	}
	// The same commit, sent, is the caller's to deliver: it holds it.
	mine.git("fetch", "--quiet", secret.dir, "main")
	if out, err := mine.push(s, path, "sbx-child2", "widget", "sbx-caller", secretTip+":refs/heads/main"); err != nil {
		t.Fatalf("a commit the sandbox sent was refused because the cache already held it: %v\n%s", err, out)
	}

	// A pin is held to the same reach.
	result, err := s.Pin(ctx, path, "sbx-child3", "widget", map[string]string{"refs/heads/main": secretTip}, "sbx-caller")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pinned) != 0 {
		t.Fatalf("a sandbox pinned another sandbox's commit: %+v", result)
	}
	result, err = s.Pin(ctx, path, "sbx-child3", "widget", map[string]string{"refs/heads/main": base}, "sbx-caller")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Pinned, []string{"refs/heads/main"}) {
		t.Fatalf("a sandbox could not pin its own commit: %+v", result)
	}
}

func TestACacheIsNeverShallow(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	full := newWork(t)
	full.commit("a")
	full.commit("b")
	shallow := &work{t: t, dir: t.TempDir()}
	shallow.git("clone", "--quiet", "--depth=1", "file://"+full.dir, ".")
	out, err := shallow.push(s, path, "sbx-one", "widget", "", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatalf("a shallow push was accepted:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(path, "shallow")); err == nil {
		t.Fatal("the cache became shallow")
	}
}

func TestSweepRemovesTheNamespacesOfSandboxesThatAreGone(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	kept := w.commit("kept")
	if out, err := w.push(s, path, "sbx-kept", "widget", "", kept+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	w.git("checkout", "--quiet", "--orphan", "other")
	gone := w.commit("gone")
	if out, err := w.push(s, path, "sbx-gone", "widget", "", gone+":refs/heads/other"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	held := func(context.Context) ([]string, error) { return []string{"sbx-kept"}, nil }
	opts := SweepOptions{NamespaceRetention: time.Hour, CacheRetention: time.Hour}
	now := time.Now()

	// The first pass only starts the clock.
	s.Sweep(ctx, held, opts, now, logger)
	if !hasBranches(t, s, path, "sbx-gone", "widget") {
		t.Fatal("a namespace was removed before its retention")
	}
	s.Sweep(ctx, held, opts, now.Add(2*time.Hour), logger)
	if hasBranches(t, s, path, "sbx-gone", "widget") {
		t.Fatal("the namespace of a sandbox that is gone was kept")
	}
	if !hasBranches(t, s, path, "sbx-kept", "widget") {
		t.Fatal("a held sandbox's namespace was removed")
	}
	// What only the removed namespace reached is pruned; the rest is not.
	if _, err := git(ctx, path, nil, nil, "cat-file", "-e", gone); err == nil {
		t.Fatal("an unreachable commit survived the sweep")
	}
	if _, err := git(ctx, path, nil, nil, "cat-file", "-e", kept); err != nil {
		t.Fatalf("a reachable commit was pruned: %v", err)
	}

	// A cache nothing uses is removed whole once it has gone unused.
	s.Sweep(ctx, func(context.Context) ([]string, error) { return nil, nil }, opts, now.Add(4*time.Hour), logger)
	s.Sweep(ctx, func(context.Context) ([]string, error) { return nil, nil }, opts, now.Add(6*time.Hour), logger)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a cache with no namespace was kept past its retention")
	}
}

func TestAnExportedNamespaceImportsIntoAnotherCache(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	tip := w.commit("a")
	if out, err := w.push(s, path, "sbx-one", "widget", "", tip+":refs/heads/feature", tip+":refs/remotes/origin/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	dest := filepath.Join(t.TempDir(), "origins", "widget.git")
	if err := s.Export(ctx, path, "sbx-one", "widget", dest); err != nil {
		t.Fatal(err)
	}
	if got := refs(t, dest, "refs/"); !slices.Equal(got, []string{"refs/heads/feature", "refs/remotes/origin/main"}) {
		t.Fatalf("exported refs = %v", got)
	}

	other := New(filepath.Join(t.TempDir(), "caches"))
	otherPath := use(t, other, "github.com/acme/widget")
	if err := other.Import(ctx, otherPath, "sbx-one", "widget", dest); err != nil {
		t.Fatal(err)
	}
	if got := resolve(ctx, otherPath, "refs/namespaces/sbx-one-widget/refs/heads/feature"); got != tip {
		t.Fatalf("imported branch = %q, want %q", got, tip)
	}
	if got, _ := git(ctx, otherPath, nil, nil, "symbolic-ref", "refs/namespaces/sbx-one-widget/HEAD"); got != "refs/namespaces/sbx-one-widget/refs/heads/feature" {
		t.Fatalf("imported HEAD = %q", got)
	}
}

// An imported origin is whatever an archive's author wrote, configuration
// included, and the pool agent is root: git is never run in it, so nothing
// its configuration names is run, and nothing it links to elsewhere on this
// host is lent to the cache.
func TestAnImportRunsNothingTheImportedRepositoryNames(t *testing.T) {
	ctx := context.Background()
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	tip := w.commit("a")
	w.git("tag", "-a", "-m", "v1", "v1")
	w.git("pack-refs", "--all")
	src := filepath.Join(t.TempDir(), "widget.git")
	w.git("clone", "--quiet", "--bare", w.dir, src)

	// Configuration that would run a program were git started in src.
	ran := filepath.Join(t.TempDir(), "ran")
	hostile := "[core]\n\tfsmonitor = touch " + ran + "\n\tsshCommand = touch " + ran + "\n[uploadpack]\n\tpackObjectsHook = touch " + ran + "\n"
	config, err := os.OpenFile(filepath.Join(src, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.WriteString(hostile); err != nil {
		t.Fatal(err)
	}
	_ = config.Close()

	if err := s.Import(ctx, path, "sbx-one", "widget", src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("importing a repository ran a program its configuration named")
	}
	if got := resolve(ctx, path, "refs/namespaces/sbx-one-widget/refs/heads/main"); got != tip {
		t.Fatalf("imported branch = %q, want %q", got, tip)
	}
	if got := resolve(ctx, path, "refs/namespaces/sbx-one-widget/refs/tags/v1^{commit}"); got != tip {
		t.Fatalf("imported packed tag = %q, want %q", got, tip)
	}
	// The objects are the cache's own now, not borrowed.
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, path, nil, nil, "cat-file", "-e", tip+"^{tree}"); err != nil {
		t.Fatalf("the imported objects went with the repository they came from: %v", err)
	}

	// An object that is not what its name says is refused, not copied in:
	// the cache is shared, and would serve it under that name to everyone.
	forged := filepath.Join(t.TempDir(), "forged.git")
	other := newWork(t)
	other.commit("first")
	blob := other.git("rev-parse", "HEAD:first")
	if err := os.WriteFile(filepath.Join(other.dir, "second"), []byte("something else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	impostor := other.git("hash-object", "-w", "second")
	other.git("clone", "--quiet", "--bare", "--no-local", other.dir, forged)
	loose := func(dir, object string) string { return filepath.Join(dir, "objects", object[:2], object[2:]) }
	if err := os.MkdirAll(filepath.Dir(loose(forged, blob)), 0o755); err != nil {
		t.Fatal(err)
	}
	packs, _ := filepath.Glob(filepath.Join(forged, "objects", "pack", "*"))
	for _, pack := range packs {
		_ = os.Remove(pack)
	}
	for _, object := range strings.Fields(other.git("rev-list", "--objects", "--no-object-names", "HEAD")) {
		data, err := os.ReadFile(loose(filepath.Join(other.dir, ".git"), object))
		if err != nil {
			t.Fatal(err)
		}
		if object == blob {
			if data, err = os.ReadFile(loose(filepath.Join(other.dir, ".git"), impostor)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(loose(forged, object)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loose(forged, object), data, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Import(ctx, path, "sbx-forged", "widget", forged); err == nil || !strings.Contains(err.Error(), "not what their names say") {
		t.Logf("import error: %v", err)
		t.Fatal("a repository holding one blob under another's name was imported")
	}
	if _, err := git(ctx, path, nil, nil, "cat-file", "-e", blob); err == nil {
		t.Fatal("the forged object is in the cache")
	}

	// A repository that borrows objects from elsewhere on this host, by an
	// alternates file or a link, is refused: it would hand the importer
	// whatever those objects are.
	for name, corrupt := range map[string]func(string) error{
		"an alternates file": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(filepath.Join(path, "objects")+"\n"), 0o600)
		},
		"a linked object directory": func(dir string) error {
			return os.Symlink(filepath.Join(path, "objects", "pack"), filepath.Join(dir, "objects", "borrowed"))
		},
		"a linked ref": func(dir string) error {
			return os.Symlink("/etc/hostname", filepath.Join(dir, "refs", "heads", "linked"))
		},
	} {
		borrowed := filepath.Join(t.TempDir(), "borrowed.git")
		w.git("clone", "--quiet", "--bare", w.dir, borrowed)
		if err := os.MkdirAll(filepath.Join(borrowed, "objects", "info"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := corrupt(borrowed); err != nil {
			t.Fatal(err)
		}
		if err := s.Import(ctx, path, "sbx-two", "widget", borrowed); err == nil {
			t.Fatalf("a repository with %s was imported", name)
		}
		if hasBranches(t, s, path, "sbx-two", "widget") {
			t.Fatalf("a repository with %s left refs behind", name)
		}
	}
}

// A namespace whose owner marker a hard stop left empty has an unknown
// owner, not none: Sweep keeps its refs rather than remove a live sandbox's.
func TestSweepKeepsANamespaceWhoseOwnerMarkerIsEmpty(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	w := newWork(t)
	if out, err := w.push(s, path, "sbx-live", "widget", "", w.commit("live")+":refs/heads/main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(path, namespacesDir, Namespace("sbx-live", "widget")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	held := func(context.Context) ([]string, error) { return []string{"sbx-live"}, nil }
	opts := SweepOptions{NamespaceRetention: time.Hour, CacheRetention: time.Hour}
	now := time.Now()
	s.Sweep(ctx, held, opts, now, logger)
	s.Sweep(ctx, held, opts, now.Add(2*time.Hour), logger)
	if !hasBranches(t, s, path, "sbx-live", "widget") {
		t.Fatal("a namespace whose owner marker is empty was removed")
	}
}

// A marker's staged write a hard stop left behind is no namespace: the
// sweep removes it, and it keeps no cache from expiring.
func TestSweepRemovesAMarkersLeftoverStagedWrite(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	s := New(filepath.Join(t.TempDir(), "caches"))
	path := use(t, s, "github.com/acme/widget")
	leftover := filepath.Join(path, namespacesDir, ".sbx-gone-widget.123.tmp")
	if err := os.MkdirAll(filepath.Dir(leftover), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte("sbx-gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	none := func(context.Context) ([]string, error) { return nil, nil }
	opts := SweepOptions{NamespaceRetention: time.Hour, CacheRetention: time.Hour}
	now := time.Now()
	s.Sweep(ctx, none, opts, now, logger)
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the leftover staged write is still there: %v", err)
	}
	s.Sweep(ctx, none, opts, now.Add(2*time.Hour), logger)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a cache whose only entry was a leftover staged write was kept past its retention")
	}
}
