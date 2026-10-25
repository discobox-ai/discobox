package sandboxcreate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi" //nolint:gosec // a test double for a pool's git backend, on a current Go
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
)

// fakePool stands in for the control plane and one pool: a single bare
// repository holding a Git namespace per sandbox source, as a pool's cache of
// one remote does, with the pin, the origin and the cache-remote routes a
// delivery uses. It counts what a delivery sent so a test can say a second
// sandbox cost nothing.
type fakePool struct {
	t      *testing.T
	cache  string
	server *httptest.Server
	// notReady answers that many pins with the conflict a pool still coming
	// up gives.
	notReady atomic.Int32
	pins     atomic.Int32
	pushes   atomic.Int32
	fetches  atomic.Int32

	mu        sync.Mutex
	completed map[string]map[string]string
	delivered map[string]bool
}

var fakePoolRoute = regexp.MustCompile(`^/projects/p/sandboxes/([a-z0-9-]+)/(git-origins|git-caches)/([a-z0-9-]+)\.git(/.*)$`)

func newFakePool(t *testing.T) *fakePool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	p := &fakePool{t: t, cache: filepath.Join(t.TempDir(), "cache.git"), completed: map[string]map[string]string{}, delivered: map[string]bool{}}
	deliverGit(t, "", "init", "--quiet", "--bare", p.cache)
	deliverGit(t, p.cache, "config", "http.receivepack", "true")
	deliverGit(t, p.cache, "config", "uploadpack.hideRefs", "refs/namespaces")
	gitExec := deliverGit(t, "", "--exec-path")
	backend := func(namespace string) http.Handler {
		env := []string{"GIT_PROJECT_ROOT=" + p.cache, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		if namespace != "" {
			env = append(env, "GIT_NAMESPACE="+namespace)
		}
		return &cgi.Handler{Path: filepath.Join(gitExec, "git-http-backend"), Env: env}
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match := fakePoolRoute.FindStringSubmatch(r.URL.Path)
		if match == nil || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "no such route, or no token", http.StatusNotFound)
			return
		}
		sandboxID, route, slug, suffix := match[1], match[2], match[3], match[4]
		namespace := sandboxID + "-" + slug
		request := r.Clone(r.Context())
		request.URL.Path = suffix
		switch {
		case route == "git-caches":
			p.fetches.Add(1)
			backend("").ServeHTTP(w, request)
		case suffix == "/pin":
			if p.notReady.Add(-1) >= 0 {
				http.Error(w, "sandbox pool is not reachable: pool=pool-1 state=pending ready=false", http.StatusConflict)
				return
			}
			p.pins.Add(1)
			p.pin(w, r, namespace)
		default:
			if r.Method == http.MethodPost && strings.HasSuffix(suffix, "/git-receive-pack") {
				p.pushes.Add(1)
			}
			backend(namespace).ServeHTTP(w, request)
			if r.Method == http.MethodPost && strings.HasSuffix(suffix, "/git-receive-pack") {
				p.recordRemote(namespace)
			}
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

// pin is the pool's pin as gitcache makes it: each ref whose object the cache
// holds is created, and a branch only when nothing is missing.
func (p *fakePool) pin(w http.ResponseWriter, r *http.Request, namespace string) {
	var request struct {
		Refs map[string]string `json:"refs"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result := struct {
		Pinned  []string `json:"pinned"`
		Missing []string `json:"missing"`
	}{Pinned: []string{}, Missing: []string{}}
	var held []string
	for ref, object := range request.Refs {
		//nolint:gosec // the test's own repository and an object id it sent itself
		if exec.CommandContext(r.Context(), "git", "--git-dir", p.cache, "cat-file", "-e", object).Run() == nil {
			held = append(held, ref)
		} else {
			result.Missing = append(result.Missing, ref)
		}
	}
	for _, ref := range held {
		if strings.HasPrefix(ref, "refs/heads/") && len(result.Missing) > 0 {
			result.Missing = append(result.Missing, ref)
			continue
		}
		deliverGit(p.t, p.cache, "update-ref", "refs/namespaces/"+namespace+"/"+ref, request.Refs[ref])
		result.Pinned = append(result.Pinned, ref)
	}
	p.recordRemote(namespace)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		p.t.Error(err)
	}
}

// recordRemote copies a namespace's remote branches to refs/cache, which is
// what the cache-remote route serves.
func (p *fakePool) recordRemote(namespace string) {
	prefix := "refs/namespaces/" + namespace + "/refs/remotes/origin/"
	for _, line := range strings.Split(deliverGit(p.t, p.cache, "for-each-ref", "--format=%(objectname) %(refname)", prefix), "\n") {
		object, ref, ok := strings.Cut(line, " ")
		if ok {
			deliverGit(p.t, p.cache, "update-ref", "refs/cache/heads/"+strings.TrimPrefix(ref, prefix), object)
		}
	}
}

// refs is a namespace's refs, by name, as a sandbox cloning it would see them.
func (p *fakePool) refs(sandboxID, slug string) map[string]string {
	p.t.Helper()
	prefix := "refs/namespaces/" + sandboxID + "-" + slug + "/"
	out := map[string]string{}
	for _, line := range strings.Split(deliverGit(p.t, p.cache, "for-each-ref", "--format=%(objectname) %(refname)", prefix), "\n") {
		if object, ref, ok := strings.Cut(line, " "); ok {
			out[strings.TrimPrefix(ref, prefix)] = object
		}
	}
	return out
}

func (p *fakePool) GetSandbox(_ context.Context, params apiclientgen.GetSandboxParams) (apiclientgen.GetSandboxRes, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sandbox := &apimodel.Sandbox{ID: params.SandboxId}
	if p.delivered[params.SandboxId] {
		sandbox.Runtime.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())
	}
	return sandbox, nil
}

func (p *fakePool) GetPool(context.Context, apiclientgen.GetPoolParams) (apiclientgen.GetPoolRes, error) {
	return &apimodel.Pool{}, nil
}

func (p *fakePool) CompleteSandboxSourcePush(_ context.Context, body *apimodel.CompleteSandboxSourcePushBody, params apiclientgen.CompleteSandboxSourcePushParams) (apiclientgen.CompleteSandboxSourcePushRes, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed[params.SandboxId] = map[string]string(body.Sources)
	p.delivered[params.SandboxId] = true
	return &apimodel.Sandbox{ID: params.SandboxId}, nil
}

func deliverGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func deliverCommit(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deliverGit(t, dir, "add", name)
	deliverGit(t, dir, "commit", "--quiet", "-m", name)
	return deliverGit(t, dir, "rev-parse", "HEAD")
}

// cacheSandbox is a sandbox with one cache-delivered primary source.
func cacheSandbox(id string, source apimodel.GitSource) *apimodel.Sandbox {
	source.Kind = apiclientgen.GitSourceKindGit
	source.Slug = apiclientgen.NewOptString("primary")
	source.Delivery = apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryCache)
	return &apimodel.Sandbox{ID: id, Config: apimodel.SandboxConfig{Source: apiclientgen.NewOptGitSource(source)}}
}

func branchCheckout(commit, branch string) apiclientgen.OptGitSourceCheckout {
	return apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString(commit),
		RefName: apiclientgen.NewOptString(branch),
		RefType: apiclientgen.NewOptString("branch"),
	})
}

func useClientGitCache(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "client-git")
	previous := clientGitCacheRoot
	clientGitCacheRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { clientGitCacheRoot = previous })
	return root
}

// A local checkout is delivered with its branch at the pinned commit, the
// remote's branches as the checkout knows them, and its tags; the second
// sandbox from the same commit is a pin, and sends nothing.
func TestALocalCheckoutIsDeliveredOnceAndPinnedAfter(t *testing.T) {
	pool := newFakePool(t)
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	deliverGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", upstream)
	work := t.TempDir()
	deliverGit(t, work, "init", "--quiet", "--initial-branch=main")
	deliverGit(t, work, "remote", "add", "origin", "https://git.invalid/acme/widget.git")
	deliverGit(t, work, "config", "url."+upstream+".insteadOf", "https://git.invalid/acme/widget.git")
	published := deliverCommit(t, work, "a")
	deliverGit(t, work, "tag", "v1")
	deliverGit(t, work, "push", "--quiet", "origin", "main")
	unpushed := deliverCommit(t, work, "b")

	source := apimodel.GitSource{
		LocalDirectory: apiclientgen.NewOptString(work),
		UpstreamUrl:    apiclientgen.NewOptString("https://git.invalid/acme/widget.git"),
		Checkout:       branchCheckout(unpushed, "main"),
	}
	local := NewLocalSources(map[string]string{"": work})
	var steps []Step
	report := Report(func(step Step) { steps = append(steps, step) })

	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-one", source), local, pool.server.URL, "token", report, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got := pool.refs("sbx-one", "primary")
	want := map[string]string{"refs/heads/main": unpushed, "refs/remotes/origin/main": published, "refs/tags/v1": published}
	for ref, object := range want {
		if got[ref] != object {
			t.Errorf("%s = %q, want %q (delivered %v)", ref, got[ref], object, got)
		}
	}
	if pool.pushes.Load() != 1 {
		t.Fatalf("the first delivery pushed %d times, want once", pool.pushes.Load())
	}
	if completed := pool.completed["sbx-one"]; completed["primary"] != unpushed {
		t.Fatalf("reported delivered = %v, want primary at %s", completed, unpushed)
	}
	if slices.Contains(steps, StepAwaitingSource) {
		t.Fatalf("a cache delivery waited for its sandbox to park: %v", steps)
	}
	lease := deliverGit(t, work, "rev-parse", sandboxgit.OriginLeaseRef("sbx-one", "primary", "main"))
	if lease != unpushed {
		t.Fatalf("lease = %q, want the delivered commit %q", lease, unpushed)
	}

	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-two", source), local, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver a second sandbox: %v", err)
	}
	if pool.pushes.Load() != 1 {
		t.Fatalf("a second sandbox from the same commit pushed (%d pushes), want a pin alone", pool.pushes.Load())
	}
	if got := pool.refs("sbx-two", "primary"); got["refs/heads/main"] != unpushed || got["refs/remotes/origin/main"] != published {
		t.Fatalf("pinned refs = %v", got)
	}
}

// A dirty workspace's snapshot travels with the branch, in the same push.
func TestADirtyWorkspaceSnapshotIsDeliveredWithItsBranch(t *testing.T) {
	pool := newFakePool(t)
	work := t.TempDir()
	deliverGit(t, work, "init", "--quiet", "--initial-branch=main")
	base := deliverCommit(t, work, "a")
	deliverGit(t, work, "update-ref", "refs/discobox/run/run-1", deliverGit(t, work, "commit-tree", "-p", base, "-m", "snapshot", base+"^{tree}"))
	snapshot := deliverGit(t, work, "rev-parse", "refs/discobox/run/run-1")

	source := apimodel.GitSource{
		LocalDirectory: apiclientgen.NewOptString(work),
		Checkout:       branchCheckout(base, "main"),
		Workspace: apiclientgen.NewOptGitSourceWorkspace(apimodel.GitSourceWorkspace{
			Mode:        apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeDirty),
			SnapshotRef: apiclientgen.NewOptString("refs/discobox/run/run-1"),
			BaseCommit:  apiclientgen.NewOptString(base),
		}),
	}
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-one", source), NewLocalSources(map[string]string{"": work}), pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got := pool.refs("sbx-one", "primary")
	if got["refs/heads/main"] != base || got["refs/discobox/run/run-1"] != snapshot {
		t.Fatalf("delivered refs = %v, want main at %s and the snapshot at %s", got, base, snapshot)
	}
}

// A remote-URL source is listed, fetched into this client's own cache of the
// remote only when the pool lacks it, and pushed; the next sandbox from it is
// a pin that needs no fetch and no cache; and when the remote has moved, what
// the pool holds is fetched first, so the remote is asked only for the rest.
func TestARemoteURLIsFetchedByTheClientOnlyWhenThePoolLacksIt(t *testing.T) {
	pool := newFakePool(t)
	clientCacheRoot := useClientGitCache(t)
	remoteWork := t.TempDir()
	deliverGit(t, remoteWork, "init", "--quiet", "--initial-branch=main")
	first := deliverCommit(t, remoteWork, "a")
	deliverGit(t, remoteWork, "tag", "-a", "-m", "v1", "v1")
	deliverGit(t, remoteWork, "branch", "release")
	remote := "file://" + filepath.ToSlash(remoteWork)
	remoteSource := func(commit string) apimodel.GitSource {
		source := apimodel.GitSource{Checkout: branchCheckout(commit, "main")}
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		source.URL = apiclientgen.NewOptURI(*u)
		return source
	}

	var steps []Step
	report := Report(func(step Step) { steps = append(steps, step) })
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-one", remoteSource(first)), nil, pool.server.URL, "token", report, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got := pool.refs("sbx-one", "primary")
	tag := deliverGit(t, remoteWork, "rev-parse", "refs/tags/v1")
	for ref, object := range map[string]string{"refs/heads/main": first, "refs/remotes/origin/main": first, "refs/remotes/origin/release": first, "refs/tags/v1": tag} {
		if got[ref] != object {
			t.Errorf("%s = %q, want %q (delivered %v)", ref, got[ref], object, got)
		}
	}
	if !slices.Contains(steps, StepFetchingSource) || pool.pushes.Load() != 1 {
		t.Fatalf("first delivery: steps %v, %d pushes; want a fetch and one push", steps, pool.pushes.Load())
	}

	// The pool holds everything now: no fetch, no push, and no client cache
	// needed — any client that can list the remote delivers it.
	if err := os.RemoveAll(clientCacheRoot); err != nil {
		t.Fatal(err)
	}
	steps = nil
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-two", remoteSource(first)), nil, pool.server.URL, "token", report, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver a second sandbox: %v", err)
	}
	if slices.Contains(steps, StepFetchingSource) || pool.pushes.Load() != 1 {
		t.Fatalf("second delivery: steps %v, %d pushes; want a pin alone", steps, pool.pushes.Load())
	}
	if _, err := os.Stat(clientCacheRoot); !os.IsNotExist(err) {
		t.Fatalf("a pinned delivery made a client cache: %v", err)
	}
	if got := pool.refs("sbx-two", "primary"); got["refs/heads/main"] != first {
		t.Fatalf("pinned refs = %v", got)
	}

	// The remote moves. This client has no cache of it, so what the pool
	// holds is fetched from the pool, and the remote gives only the rest.
	second := deliverCommit(t, remoteWork, "b")
	before := pool.fetches.Load()
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-three", remoteSource(second)), nil, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver after the remote moved: %v", err)
	}
	if pool.fetches.Load() == before {
		t.Fatal("the client did not fetch what the pool holds before fetching the remote")
	}
	if got := pool.refs("sbx-three", "primary"); got["refs/heads/main"] != second || got["refs/remotes/origin/main"] != second {
		t.Fatalf("delivered refs = %v, want main at %s", got, second)
	}
	caches, _ := filepath.Glob(filepath.Join(clientCacheRoot, "*.git"))
	if len(caches) != 1 {
		t.Fatalf("client caches = %v, want one for the remote", caches)
	}
	if pooled := deliverGit(t, caches[0], "rev-parse", "refs/discobox/pool/heads/main"); pooled != first {
		t.Fatalf("what the client fetched from the pool = %q, want the pool's main %q", pooled, first)
	}
}

// A remote source asked to start in a //PATH is checked against its commit
// before the pool is given it — fetched into the client's cache for that even
// when the pool needs nothing sent — and refused when the commit has no such
// directory (ADR 26-10-15-664 §3).
func TestARemoteSourcesStartingDirectoryIsCheckedAtDelivery(t *testing.T) {
	pool := newFakePool(t)
	clientCacheRoot := useClientGitCache(t)
	remoteWork := t.TempDir()
	deliverGit(t, remoteWork, "init", "--quiet", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(remoteWork, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	commit := deliverCommit(t, remoteWork, filepath.Join("app", "main.go"))
	remote := "file://" + filepath.ToSlash(remoteWork)
	startingIn := func(dir string) apimodel.GitSource {
		source := apimodel.GitSource{Checkout: branchCheckout(commit, "main")}
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		source.URL = apiclientgen.NewOptURI(*u)
		source.Destination = apiclientgen.NewOptGitSourceDestination(apimodel.GitSourceDestination{
			Directory:        apiclientgen.NewOptString("/workspace/source"),
			WorkingDirectory: apiclientgen.NewOptString("/workspace/source/" + dir),
		})
		return source
	}

	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-one", startingIn("app")), nil, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if pool.pushes.Load() != 1 {
		t.Fatalf("pushes = %d, want the one delivery", pool.pushes.Load())
	}

	// The pool holds the commit, but this client does not: it fetches to
	// look, and sends nothing.
	if err := os.RemoveAll(clientCacheRoot); err != nil {
		t.Fatal(err)
	}
	var steps []Step
	report := Report(func(step Step) { steps = append(steps, step) })
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-two", startingIn("app")), nil, pool.server.URL, "token", report, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver a second sandbox: %v", err)
	}
	if !slices.Contains(steps, StepFetchingSource) || pool.pushes.Load() != 1 {
		t.Fatalf("second delivery: steps %v, %d pushes; want a fetch to check and a pin", steps, pool.pushes.Load())
	}

	err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-three", startingIn("missing/dir")), nil, pool.server.URL, "token", nil, DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "missing/dir") {
		t.Fatalf("deliver into a directory the commit lacks = %v, want a refusal naming it", err)
	}
	if got := pool.refs("sbx-three", "primary"); len(got) != 0 {
		t.Fatalf("a refused delivery reached the pool: %v", got)
	}
	err = DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-four", startingIn("app/main.go")), nil, pool.server.URL, "token", nil, DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "app/main.go") {
		t.Fatalf("deliver into a file = %v, want a refusal naming it", err)
	}

	// A link to a directory is one the checkout follows, as a local
	// DIR//PATH's is.
	if err := os.Symlink("app", filepath.Join(remoteWork, "current")); err != nil {
		t.Fatal(err)
	}
	deliverGit(t, remoteWork, "add", "current")
	deliverGit(t, remoteWork, "commit", "--quiet", "-m", "link")
	commit = deliverGit(t, remoteWork, "rev-parse", "HEAD")
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-five", startingIn("current")), nil, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver into a linked directory: %v", err)
	}
}

// A delivery starts when the create returns, so it can beat its pool: a pool
// that is not up yet is waited for, and is the only thing that is.
func TestADeliveryWaitsOnlyForAPoolThatIsNotUpYet(t *testing.T) {
	pool := newFakePool(t)
	previous := pinPollInterval
	pinPollInterval = time.Millisecond
	t.Cleanup(func() { pinPollInterval = previous })
	work := t.TempDir()
	deliverGit(t, work, "init", "--quiet", "--initial-branch=main")
	commit := deliverCommit(t, work, "a")
	source := apimodel.GitSource{LocalDirectory: apiclientgen.NewOptString(work), Checkout: branchCheckout(commit, "main")}

	pool.notReady.Store(3)
	if err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-one", source), NewLocalSources(map[string]string{"": work}), pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver to a pool still coming up: %v", err)
	}
	if got := pool.refs("sbx-one", "primary"); got["refs/heads/main"] != commit {
		t.Fatalf("delivered refs = %v", got)
	}

	// Any other refusal is an answer.
	err := DeliverSource(t.Context(), pool, "p", cacheSandbox("sbx-two", source), NewLocalSources(map[string]string{"": work}), pool.server.URL, "wrong-token", nil, DeliveryOptions{})
	if err == nil || !strings.Contains(err.Error(), "refused the source") {
		t.Fatalf("a refused pin = %v, want the refusal", err)
	}
}

// A delivery somebody else already reported is not made again.
func TestADeliveredSandboxIsNotDeliveredAgain(t *testing.T) {
	pool := newFakePool(t)
	sandbox := cacheSandbox("sbx-one", apimodel.GitSource{LocalDirectory: apiclientgen.NewOptString(t.TempDir()), Checkout: branchCheckout(testCommit, "main")})
	sandbox.Runtime.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())
	if err := DeliverSource(t.Context(), pool, "p", sandbox, nil, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if pool.pins.Load() != 0 || len(pool.completed) != 0 {
		t.Fatal("a delivered sandbox was delivered again")
	}
}

func (p *fakePool) PullSandboxSource(context.Context, *apimodel.PullSandboxSourceBody, apiclientgen.PullSandboxSourceParams) (apiclientgen.PullSandboxSourceRes, error) {
	panic("unexpected credential loan")
}
