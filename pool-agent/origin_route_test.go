package poolagent_test

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
)

// originRoute is a pool agent with two sandboxes: sandbox-1's primary is a
// live origin, the developer's own repository, and its hooks are a bare
// origin the client pushes into.
type originRoute struct {
	server           *httptest.Server
	developer        string
	bare             string
	poolKey          ed25519.PrivateKey
	signControlPlane func(projectID, poolID, sandboxID string, scopes ...string) string
}

func newOriginRoute(t *testing.T) *originRoute {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: git line-ending translation")
	}
	runtime := poolagent.NewMemorySandboxRuntime()
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
	runtime.SetLiveGitOrigin("sandbox-1", "primary", filepath.Join(developer, ".git"), nil)
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
	return &originRoute{server: server, developer: developer, bare: bare, poolKey: poolKey, signControlPlane: signControlPlane}
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

// The sandbox clones and fetches its origins with the token its pool issued
// it, whichever kind of origin answers, and sees the developer's new commits
// without anyone pushing them.
func TestASandboxFetchesItsOriginsWithItsOwnToken(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-1")

	clone := filepath.Join(t.TempDir(), "clone")
	git(t, "", "-c", bearer(token), "clone", "-q", o.url("sandbox-1", "git-origins", "primary"), clone)
	if err := os.WriteFile(filepath.Join(o.developer, "NEW.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, o.developer, "add", "NEW.md")
	git(t, o.developer, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "two")
	git(t, clone, "-c", bearer(token), "fetch", "-q", "origin")
	if got, want := gitOutput(t, clone, "rev-parse", "origin/main"), gitOutput(t, o.developer, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin/main = %s, want the developer's new commit %s", got, want)
	}

	if out := gitOutput(t, "", "-c", bearer(token), "ls-remote", o.url("sandbox-1", "git-origins", "hooks")); out != "" {
		t.Fatalf("an empty bare origin advertised %q", out)
	}
}

// The sandbox's token pushes nothing — not into the developer's repository and
// not into the bare origin the client pushes into — and a control-plane token
// that may write still cannot push into a live origin.
func TestNothingPushesThroughTheOriginRouteButTheClient(t *testing.T) {
	o := newOriginRoute(t)
	token := o.sandboxToken(t, "sandbox-1")
	write := o.signControlPlane("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)

	clone := filepath.Join(t.TempDir(), "clone")
	git(t, "", "-c", bearer(token), "clone", "-q", o.url("sandbox-1", "git-origins", "primary"), clone)
	git(t, clone, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "sandbox work")
	before := gitOutput(t, o.developer, "for-each-ref")
	for name, push := range map[string][]string{
		"the sandbox's token into the live origin": {"-c", bearer(token), "push", o.url("sandbox-1", "git-origins", "primary"), "HEAD:refs/heads/main"},
		"a write token into the live origin":       {"-c", bearer(write), "push", o.url("sandbox-1", "git-origins", "primary"), "HEAD:refs/heads/pushed"},
		"the sandbox's token into the bare origin": {"-c", bearer(token), "push", o.url("sandbox-1", "git-origins", "hooks"), "HEAD:refs/heads/main"},
	} {
		if err := gitErr(clone, push...); err == nil {
			t.Fatalf("%s succeeded", name)
		}
	}
	if after := gitOutput(t, o.developer, "for-each-ref"); after != before {
		t.Fatalf("the developer's refs changed:\n%s\nwant\n%s", after, before)
	}
	if out := gitOutput(t, o.bare, "for-each-ref"); out != "" {
		t.Fatalf("the bare origin gained refs: %s", out)
	}

	// The client still delivers into the bare origin as it always has.
	git(t, clone, "-c", bearer(write), "push", "-q", o.url("sandbox-1", "git-origins", "hooks"), "HEAD:refs/heads/main")
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
