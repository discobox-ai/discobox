package sandboxruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Each source is named with where the sandbox fetches it from: a source on
// the client's machine, live or pushed, at this pool's origins host, and a
// remote-URL source at its remote. Whether it was delivered, and its token,
// carry over from the document as recorded.
func TestRuntimeSourcesNameWhereEachOriginIs(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	remote, err := url.Parse("https://github.com/example/remote.git")
	if err != nil {
		t.Fatal(err)
	}
	sources := []sandboxSource{
		{slug: "primary", target: "/workspace", git: workerapimodel.GitSource{
			Kind: workerclient.GitSourceKindGit, LocalDirectory: workerclient.NewOptString("/src/app"),
			Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{Commit: workerclient.NewOptString("abc123")}),
		}},
		{slug: "hooks", target: "/workspace/hooks", git: pushDeliveredSource("main")},
		{slug: "lib", target: "/workspace/lib", git: workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit, URL: workerclient.NewOptURI(*remote)}},
	}
	prior := []sandboxconfig.RuntimeSource{
		{Slug: "primary", OriginURL: proxyagent.OriginURL("proj_a", "pool_a", "sbx_1", "primary"), OriginToken: "kept", Delivered: true},
		{Slug: "hooks", OriginURL: "/.discobox/origins/hooks", OriginToken: "stale"},
	}
	got := r.runtimeSources("sbx_1", sources, prior)
	want := []sandboxconfig.RuntimeSource{
		{Slug: "primary", Target: "/workspace", OriginURL: "https://git.discobox.internal/api/project/proj_a/pool/pool_a/sandboxes/sbx_1/git-origins/primary.git", OriginToken: "kept", Commit: "abc123", Delivered: true},
		{Slug: "hooks", Target: "/workspace/hooks", OriginURL: "https://git.discobox.internal/api/project/proj_a/pool/pool_a/sandboxes/sbx_1/git-origins/hooks.git"},
		{Slug: "lib", Target: "/workspace/lib", OriginURL: "https://github.com/example/remote.git"},
	}
	if len(got) != len(want) {
		t.Fatalf("sources = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Deciding the document gives every origin this pool serves a token that
// fetches this sandbox's origins and nothing else, keeps it while it has long
// to live — so deciding again is not a new revision — renews it near its end,
// and gives a remote none.
func TestOriginTokensAreIssuedKeptAndRenewed(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	setSources := func(doc *sandboxconfig.RuntimeConfig) {
		doc.Sources = []sandboxconfig.RuntimeSource{
			{Slug: "primary", Target: "/workspace", OriginURL: proxyagent.OriginURL("proj_a", "pool_a", "sbx_1", "primary")},
			{Slug: "lib", Target: "/workspace/lib", OriginURL: "https://github.com/example/remote.git", OriginToken: "not-ours"},
		}
	}
	doc, err := r.decideRuntimeConfig("sbx_1", setSources)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := r.identityKey.Public().(ed25519.PublicKey)
	verifier, err := sandboxtoken.NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(doc.Sources[0].OriginToken)
	if err != nil {
		t.Fatalf("the origin token does not verify: %v", err)
	}
	if claims.SandboxID != "sbx_1" || claims.PoolID != "pool_a" || len(claims.Scopes) != 1 || claims.Scopes[0] != sandboxtoken.ScopeOriginFetch {
		t.Fatalf("origin token claims = %+v", claims)
	}
	if doc.Sources[1].OriginToken != "" {
		t.Fatal("a remote's origin was given a token")
	}

	again, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != doc.Revision || again.Sources[0].OriginToken != doc.Sources[0].OriginToken {
		t.Fatalf("deciding again = revision %d, a new token %v; want the same document", again.Revision, again.Sources[0].OriginToken != doc.Sources[0].OriginToken)
	}

	// A token near its end is replaced.
	expiring, err := sandboxtoken.Issue(r.identityKey, sandboxtoken.Claims{ProjectID: "proj_a", PoolID: "pool_a", SandboxID: "sbx_1", Scopes: []string{sandboxtoken.ScopeOriginFetch}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.decideRuntimeConfig("sbx_1", func(doc *sandboxconfig.RuntimeConfig) { doc.Sources[0].OriginToken = expiring }); err != nil {
		t.Fatal(err)
	}
	renewed, err := r.readRuntimeConfigRecord(t)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Sources[0].OriginToken == expiring {
		t.Fatal("a token an hour from its end was kept")
	}
	if claims, err := verifier.Verify(renewed.Sources[0].OriginToken); err != nil || time.Until(claims.Expires) < originTokenRenewBefore {
		t.Fatalf("the renewed token = %+v, %v", claims, err)
	}
}

func (r *DockerSandboxRuntime) readRuntimeConfigRecord(t *testing.T) (sandboxconfig.RuntimeConfig, error) {
	t.Helper()
	doc, _, err := r.readRuntimeConfig("sbx_1")
	return doc, err
}

// fakeSourcesAgent is the sandbox agent's two source routes: it checks the
// pool-signed token the way the agent does, and answers with the states it is
// told to, in order, and with a project layer.
type fakeSourcesAgent struct {
	t     *testing.T
	key   ed25519.PublicKey
	layer string

	mu     sync.Mutex
	states [][]sourceState
	reads  int
}

func (f *fakeSourcesAgent) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
		return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
	}
	public, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience("sandbox-agent"))
	token, err := parser.ParseV4Public(public, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var scopes []string
	_ = token.Get("scopes", &scopes)
	if len(scopes) != 1 || scopes[0] != sandboxconfig.RuntimeConfigScope {
		http.Error(w, "scope", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.URL.Path {
	case "/api/projects/proj_a/sandboxes/sbx_1/sources":
		f.mu.Lock()
		states := f.states[min(f.reads, len(f.states)-1)]
		f.reads++
		f.mu.Unlock()
		if err := json.NewEncoder(w).Encode(struct {
			Sources []sourceState `json:"sources"`
		}{states}); err != nil {
			f.t.Error(err)
		}
	case "/api/projects/proj_a/sandboxes/sbx_1/sources/primary/project-layer":
		body := struct {
			Slug         string          `json:"slug"`
			Commit       string          `json:"commit"`
			ProjectLayer json.RawMessage `json:"projectLayer,omitempty"`
		}{Slug: "primary", Commit: "abc123"}
		if f.layer != "" {
			body.ProjectLayer = json.RawMessage(f.layer)
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			f.t.Error(err)
		}
	default:
		f.t.Errorf("request %s %s", req.Method, req.URL.Path)
		http.NotFound(w, req)
	}
}

func newFakeSourcesAgent(t *testing.T, r *DockerSandboxRuntime, layer string, states ...[]sourceState) (*fakeSourcesAgent, Dialer) {
	t.Helper()
	public, _ := r.identityKey.Public().(ed25519.PublicKey)
	agent := &fakeSourcesAgent{t: t, key: public, layer: layer, states: states}
	return agent, intakeDialer(t, agent)
}

// A create waits until the sandbox reports every source materialized.
func TestAwaitSourcesMaterializedWaitsForEverySource(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	at := time.Now()
	agent, dial := newFakeSourcesAgent(t, r, "",
		[]sourceState{{Slug: "primary", State: "waiting", UpdatedAt: at}},
		[]sourceState{{Slug: "primary", State: "cloning", UpdatedAt: at.Add(time.Second)}, {Slug: "lib", State: "materialized"}},
		[]sourceState{{Slug: "primary", State: "materialized", UpdatedAt: at.Add(2 * time.Second)}, {Slug: "lib", State: "materialized"}},
	)
	sources := []sandboxconfig.RuntimeSource{{Slug: "primary"}, {Slug: "lib"}}
	if err := r.awaitSourcesMaterialized(context.Background(), dial, "sbx_1", sources); err != nil {
		t.Fatalf("await = %v", err)
	}
	if agent.reads != 3 {
		t.Fatalf("the sandbox was asked %d times, want until both were materialized (3)", agent.reads)
	}
}

// A source the sandbox keeps failing to clone fails the create with what the
// sandbox said, once it has failed attempts the wait itself saw — not on a
// failure from before the wait woke the agent.
func TestAwaitSourcesMaterializedGivesUpOnASourceThatKeepsFailing(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	at := time.Now()
	failed := func(n int) []sourceState {
		return []sourceState{{Slug: "primary", State: "failed", Error: "fatal: repository not found", UpdatedAt: at.Add(time.Duration(n) * time.Second)}}
	}
	agent, dial := newFakeSourcesAgent(t, r, "", failed(0), failed(1), failed(2), failed(3))
	err := r.awaitSourcesMaterialized(context.Background(), dial, "sbx_1", []sandboxconfig.RuntimeSource{{Slug: "primary"}})
	if err == nil || !strings.Contains(err.Error(), "repository not found") {
		t.Fatalf("await = %v, want the sandbox's failure", err)
	}
	if agent.reads != 4 {
		t.Fatalf("gave up after %d reads, want the earlier failure and %d more", agent.reads, sourceFailureLimit)
	}
}

// Settling reads the primary source's project layer from the sandbox and asks
// for a rebuild only when it is not the layer the container was built with,
// recording it for that rebuild.
func TestSettleProjectLayerRebuildsOnlyForALayerTheContainerLacks(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	_, dial := newFakeSourcesAgent(t, r, `{"runCommand":["./run.sh"]}`, nil)
	rebuild, err := r.settleProjectLayer(context.Background(), dial, "sbx_1", "primary", "")
	if err != nil || !rebuild {
		t.Fatalf("settle against a container built with no layer = %v, %v; want a rebuild", rebuild, err)
	}
	record, err := r.readProjectLayerRecord("sbx_1")
	if err != nil || record.Project == nil || len(record.Project.RunCommand) != 1 || record.Project.RunCommand[0] != "./run.sh" || record.Source == nil || *record.Source != "primary" {
		t.Fatalf("record = %+v, %v; want the layer the sandbox read", record, err)
	}
	rebuild, err = r.settleProjectLayer(context.Background(), dial, "sbx_1", "primary", projectLayerDigest(record.Project))
	if err != nil || rebuild {
		t.Fatalf("settle against the rebuilt container = %v, %v; want it final", rebuild, err)
	}

	// A sandbox with no primary source has no layer to read.
	if rebuild, err := r.settleProjectLayer(context.Background(), dial, "sbx_1", "", ""); err != nil || rebuild {
		t.Fatalf("settle with no primary source = %v, %v", rebuild, err)
	}
}

// A container built before the label existed carries none, which reads as no
// layer — what every such container was built with.
func TestProjectLayerDigestOfNoneIsEmpty(t *testing.T) {
	if projectLayerDigest(nil) != "" {
		t.Fatal("no layer has a digest")
	}
	a := projectLayerDigest(&sandboxconfig.ProjectLayer{RunCommand: []string{"a"}})
	b := projectLayerDigest(&sandboxconfig.ProjectLayer{RunCommand: []string{"b"}})
	if a == "" || a == b {
		t.Fatalf("digests %q and %q", a, b)
	}
}

// An agent that cannot report its sources — an image from before the route,
// or one with no converger — fails a create at once, not after the whole wait.
func TestAwaitSourcesMaterializedFailsAtOnceOnAnAgentWithoutTheRoute(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		dial := intakeDialer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", status)
		}))
		started := time.Now()
		err := r.awaitSourcesMaterialized(context.Background(), dial, "sbx_1", []sandboxconfig.RuntimeSource{{Slug: "primary"}})
		if !errors.Is(err, ErrSourceStatesUnsupported) || time.Since(started) > 5*time.Second {
			t.Fatalf("status %d: await = %v after %s, want ErrSourceStatesUnsupported at once", status, err, time.Since(started))
		}
	}
}

// A source that names its branch has its origin's HEAD there from the moment
// the origin is made, so a clone made the moment the push lands resolves
// origin/HEAD; one that names none has HEAD pointed at what arrived when it
// is settled, by whichever settles it first.
func TestPushedOriginsHeadAtTheirBranchOrWhatArrived(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := deliveryTestRuntime(t)
	named := runtime.sandboxOriginPath(deliveryTestSandboxID, "named")
	if err := runtime.initGitOrigin(ctx, named, "feature", currentUser()); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, named, "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("a named branch's origin HEAD = %q before any push", got)
	}
	unnamed := runtime.sandboxOriginPath(deliveryTestSandboxID, "unnamed")
	if err := runtime.initGitOrigin(ctx, unnamed, "", currentUser()); err != nil {
		t.Fatal(err)
	}
	pushed := pushCommitToOrigin(t, unnamed, "discobox-source")
	sources := []sandboxconfig.RuntimeSource{{Slug: "named"}, {Slug: "unnamed"}, {Slug: "remote"}}
	if err := runtime.headPushedOrigins(ctx, deliveryTestSandboxID, sources); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, unnamed, "rev-parse", "HEAD"); got != pushed {
		t.Fatalf("an unnamed branch's origin HEAD = %q, want what was pushed %q", got, pushed)
	}
}
