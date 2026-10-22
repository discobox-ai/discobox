package poolagent_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/proxy"
)

// originCacheKey is the cache sandbox-1's primary source is delivered into.
const originCacheKey = "remote/example.com/acme/project"

// originRoute is a pool agent with two sandboxes: sandbox-1's primary is a
// namespace of the pool's Git cache, which a client delivers into, and its
// hooks are a bare origin of its own, as a source delivered before the cache
// has.
type originRoute struct {
	runtime          *poolagent.MemorySandboxRuntime
	server           *httptest.Server
	developer        string
	bare             string
	poolKey          ed25519.PrivateKey
	controlPlaneKey  string
	signControlPlane func(projectID, poolID, sandboxID string, scopes ...string) string
}

func newOriginRoute(t *testing.T) *originRoute {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: git line-ending translation")
	}
	runtime := poolagent.NewMemorySandboxRuntime()
	runtime.SetGitCacheRoot(filepath.Join(t.TempDir(), "git-cache"))
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		if _, err := runtime.CreateSandbox(t.Context(), &workerapimodel.PoolSandboxCreateRequest{
			SandboxId: id,
			Config:    workerapimodel.SandboxConfig{Image: workerclient.NewOptString("alpine")},
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	developer := filepath.Join(t.TempDir(), "project")
	initGitRepo(t, developer, "one\n")
	runtime.SetSourceCache("sandbox-1", "primary", originCacheKey)
	bare := filepath.Join(t.TempDir(), "hooks.git")
	git(t, "", "init", "--bare", "-b", "main", bare)
	runtime.SetGitOriginPath("sandbox-1", "hooks", bare)

	_, poolKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	controlPlaneKey, signControlPlane := workerAgentTestSigner(t)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
		SandboxTokenKey:       poolKey.Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &originRoute{runtime: runtime, server: server, developer: developer, bare: bare, poolKey: poolKey, controlPlaneKey: controlPlaneKey, signControlPlane: signControlPlane}
}

func (o *originRoute) url(sandboxID, route, slug string) string {
	return o.server.URL + "/api/project/project-1/pool/pool-1/sandboxes/" + sandboxID + "/" + route + "/" + slug + ".git"
}

func (o *originRoute) sandboxToken(t *testing.T, sandboxID string) string {
	t.Helper()
	token, err := sandboxtoken.Issue(o.poolKey, sandboxtoken.Claims{
		ProjectID: "project-1",
		PoolID:    "pool-1",
		SandboxID: sandboxID,
		Scopes:    []string{sandboxtoken.ScopeOriginFetch},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func bearer(token string) string {
	return "http.extraHeader=Authorization: Bearer " + token
}

// asControlPlane is the switches a git command reaches a cache route with, as
// the control plane forwards a client's request: its token for sandboxID, and
// the cache it names.
func (o *originRoute) asControlPlane(sandboxID string) []string {
	token := o.signControlPlane("project-1", "pool-1", sandboxID, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	return []string{"-c", bearer(token), "-c", "http.extraHeader=" + sourcecache.KeyHeader + ": " + originCacheKey}
}

// deliver pushes the developer's HEAD into a sandbox's primary namespace, as
// a client's delivery does.
func (o *originRoute) deliver(t *testing.T, sandboxID string) string {
	t.Helper()
	git(t, o.developer, append(o.asControlPlane(sandboxID), "push", "-q", o.url(sandboxID, sourcecache.PoolOriginRoute, "primary"), "HEAD:refs/heads/main", "HEAD:refs/remotes/origin/main")...)
	return gitOutput(t, o.developer, "rev-parse", "HEAD")
}

// pin asks the pool to create refs in a sandbox's primary namespace from what
// its cache holds, with the headers given besides the control plane's own.
func (o *originRoute) pin(t *testing.T, sandboxID string, refs map[string]string, headers map[string]string) (status int, result struct{ Pinned, Missing []string }) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"refs": refs})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, o.url(sandboxID, sourcecache.PoolOriginRoute, "primary")+sourcecache.PinSuffix, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+o.signControlPlane("project-1", "pool-1", sandboxID, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite))
	req.Header.Set(sourcecache.KeyHeader, originCacheKey)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, result
}

// The sandbox clones and fetches its origins with the token its pool issued
// it, whichever kind of origin answers, and sees its client's new commits once
// the client has pushed them.
func TestASandboxFetchesItsOriginsWithItsOwnToken(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-1")
	o.deliver(t, "sandbox-1")

	clone := filepath.Join(t.TempDir(), "clone")
	git(t, "", "-c", bearer(token), "clone", "-q", o.url("sandbox-1", "git-origins", "primary"), clone)
	if err := os.WriteFile(filepath.Join(o.developer, "NEW.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, o.developer, "add", "NEW.md")
	git(t, o.developer, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "two")
	git(t, clone, "-c", bearer(token), "fetch", "-q", "origin")
	if got, want := gitOutput(t, clone, "rev-parse", "origin/main"), gitOutput(t, o.developer, "rev-parse", "HEAD"); got == want {
		t.Fatalf("origin/main = %s: the sandbox saw a commit nobody pushed", got)
	}
	want := o.deliver(t, "sandbox-1")
	git(t, clone, "-c", bearer(token), "fetch", "-q", "origin")
	if got := gitOutput(t, clone, "rev-parse", "origin/main"); got != want {
		t.Fatalf("origin/main = %s, want the client's new commit %s", got, want)
	}

	if out := gitOutput(t, "", "-c", bearer(token), "ls-remote", o.url("sandbox-1", "git-origins", "hooks")); out != "" {
		t.Fatalf("an empty bare origin advertised %q", out)
	}
}

// The sandbox's token pushes nothing — not into its namespace of the cache
// and not into a bare origin — and the origin route serves a namespace
// fetch-only whoever asks: a delivery arrives on the cache route.
func TestNothingPushesThroughTheOriginRouteButTheClient(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-1")
	write := o.signControlPlane("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	delivered := o.deliver(t, "sandbox-1")

	clone := filepath.Join(t.TempDir(), "clone")
	git(t, "", "-c", bearer(token), "clone", "-q", o.url("sandbox-1", "git-origins", "primary"), clone)
	git(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "sandbox work")
	for name, push := range map[string][]string{
		"the sandbox's token into its namespace":      {"-c", bearer(token), "push", o.url("sandbox-1", "git-origins", "primary"), "HEAD:refs/heads/main"},
		"a write token through the origin route":      {"-c", bearer(write), "push", o.url("sandbox-1", "git-origins", "primary"), "HEAD:refs/heads/pushed"},
		"the sandbox's token into the bare origin":    {"-c", bearer(token), "push", o.url("sandbox-1", "git-origins", "hooks"), "HEAD:refs/heads/main"},
		"the sandbox's token through the cache route": {"-c", bearer(token), "-c", "http.extraHeader=" + sourcecache.KeyHeader + ": " + originCacheKey, "push", o.url("sandbox-1", sourcecache.PoolOriginRoute, "primary"), "HEAD:refs/heads/main"},
	} {
		if err := gitErr(clone, push...); err == nil {
			t.Fatalf("%s succeeded", name)
		}
	}
	if got := gitOutput(t, "", "-c", bearer(token), "ls-remote", o.url("sandbox-1", "git-origins", "primary"), "refs/heads/*"); !strings.HasPrefix(got, delivered) || strings.Count(got, "\n") != 0 {
		t.Fatalf("the namespace's branches changed: %q", got)
	}
	if out := gitOutput(t, o.bare, "for-each-ref"); out != "" {
		t.Fatalf("the bare origin gained refs: %s", out)
	}

	// The client still delivers into the bare origin as it always has.
	git(t, clone, "-c", bearer(write), "push", "-q", o.url("sandbox-1", "git-origins", "hooks"), "HEAD:refs/heads/main")
}

// A delivery names its cache and its namespace, and needs nothing else of the
// pool: no record of the sandbox and no container. The next sandbox from the
// same commit is a pin, which transfers nothing.
func TestADeliveryNeedsNoSandboxAndASecondOneIsAPin(t *testing.T) {
	o := newOriginRoute(t)
	commit := o.deliver(t, "sandbox-not-created-yet")

	refs := map[string]string{"refs/heads/main": commit, "refs/remotes/origin/main": commit}
	status, result := o.pin(t, "sandbox-later", refs, nil)
	if status != http.StatusOK || len(result.Missing) != 0 || len(result.Pinned) != 2 {
		t.Fatalf("pin = %d %+v, want both refs pinned", status, result)
	}
	// What the cache remembers of the remote is a member's to fetch, and
	// holds no namespace.
	remote := gitOutput(t, "", append(o.asControlPlane("sandbox-later"), "ls-remote", o.url("sandbox-later", sourcecache.PoolRemoteRoute, "primary"))...)
	if want := commit + "\trefs/cache/heads/main"; remote != want {
		t.Fatalf("the cache's record of its remote = %q, want %q", remote, want)
	}

	// A sandbox delivering to a discobox it created is held to what its own
	// sources hold: this one has none here, so nothing is pinned for it, and
	// a cache's record of its remote is not its to fetch.
	status, result = o.pin(t, "sandbox-child", refs, map[string]string{sourcecache.CallerHeader: "sandbox-2"})
	if status != http.StatusOK || len(result.Pinned) != 0 {
		t.Fatalf("a sandbox's pin = %d %+v, want nothing pinned", status, result)
	}
	if err := gitErr("", append(o.asControlPlane("sandbox-later"), "-c", "http.extraHeader="+sourcecache.CallerHeader+": sandbox-2", "ls-remote", o.url("sandbox-later", sourcecache.PoolRemoteRoute, "primary"))...); err == nil {
		t.Fatal("a sandbox fetched a cache's record of its remote")
	}
	// Without a cache named there is nothing to serve.
	if status, _ := o.pin(t, "sandbox-later", refs, map[string]string{sourcecache.KeyHeader: ""}); status != http.StatusBadRequest {
		t.Fatalf("a pin naming no cache = %d, want 400", status)
	}
}

// A sandbox's token is good for its own origins and for nothing else this
// agent serves: not another sandbox's, and not any route the control plane's
// tokens reach.
func TestASandboxTokenReachesOnlyItsOwnOrigins(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-2")

	if err := gitErr("", "-c", bearer(token), "ls-remote", o.url("sandbox-1", "git-origins", "primary")); err == nil {
		t.Fatal("sandbox-2's token read sandbox-1's origin")
	}
	for _, path := range []string{
		"/api/project/project-1/pool/pool-1/sandboxes/sandbox-2",
		"/api/project/project-1/pool/pool-1/sandboxes/sandbox-2/git-repositories/primary.git/info/refs?service=git-upload-pack",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, o.server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with a sandbox token = %d, want 401", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
		}
	}
}

// The token is checked against the path the router serves. An escaped slash
// in an id makes the decoded path name a different sandbox than the router
// does: here it names the token's own sandbox-2, while the route, and the
// on-demand start in front of it, would act on sandbox-1.
func TestASandboxTokenCannotReachAnotherSandboxThroughAnEscapedPath(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-2")
	if err := o.runtime.StopSandbox(t.Context(), "sandbox-1", nil); err != nil {
		t.Fatal(err)
	}

	smuggled := o.server.URL + "/api/project/project-1%2Fpool%2Fpool-1%2Fsandboxes%2Fsandbox-2/pool/pool-1/sandboxes/sandbox-1/git-origins/primary.git/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, smuggled, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sandbox-2's token on an escaped path to sandbox-1 = %d, want 403", resp.StatusCode)
	}
	sandbox, err := o.runtime.GetSandbox(t.Context(), "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Status != sandboxruntime.StatusStopped {
		t.Fatalf("sandbox-1 is %s after another sandbox's request, want it left stopped", sandbox.Status)
	}
}

// The origin listener the pool proxy forwards to serves a sandbox's fetch only
// with that sandbox's own token and only when the proxy names the same
// sandbox: a token copied out of one sandbox reads nothing from another, and
// the control plane's tokens, which reach the same route on the agent's own
// listener, are not taken here.
func TestTheOriginListenerServesTheSandboxTheProxyNames(t *testing.T) {
	o := newOriginRoute(t)
	router, err := poolagentserver.NewOriginRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               o.runtime,
		ControlPlanePublicKey: o.controlPlaneKey,
		SandboxTokenKey:       o.poolKey.Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(router)
	t.Cleanup(listener.Close)
	refs := listener.URL + "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/git-origins/primary.git/info/refs?service=git-upload-pack"
	status := func(token, client string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, refs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if client != "" {
			req.Header.Set(proxy.PoolClientHeader, client)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	own := o.sandboxToken(t, "sandbox-1")
	if got := status(own, "sandbox-1"); got != http.StatusOK {
		t.Fatalf("sandbox-1's token, as sandbox-1 = %d, want 200", got)
	}
	for name, call := range map[string]struct {
		token, client string
		want          int
	}{
		"no proxy assertion":            {own, "", http.StatusForbidden},
		"another sandbox's certificate": {own, "sandbox-2", http.StatusForbidden},
		"another sandbox's token":       {o.sandboxToken(t, "sandbox-2"), "sandbox-2", http.StatusForbidden},
		"no token":                      {"", "sandbox-1", http.StatusUnauthorized},
		"a control-plane token":         {o.signControlPlane("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead), "sandbox-1", http.StatusUnauthorized},
	} {
		if got := status(call.token, call.client); got != call.want {
			t.Errorf("%s = %d, want %d", name, got, call.want)
		}
	}
}
