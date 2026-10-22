package sandboxruntime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// originTestRequest is a sandbox with a cache-delivered primary from a
// checkout at local, on a branch with a dirty workspace, and a reference
// pushed into a bare origin of its own, as a source delivered before a pool
// cached Git is.
func originTestRequest(local string) *workerapimodel.PoolSandboxCreateRequest {
	return &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: deliveryTestSandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind:           workerclient.GitSourceKindGit,
				Slug:           workerclient.NewOptString("primary"),
				LocalDirectory: workerclient.NewOptString(local),
				Delivery:       workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryCache),
				CacheKey:       workerclient.NewOptString(originTestCacheKey),
				Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
					RefType: workerclient.NewOptString("branch"),
					RefName: workerclient.NewOptString("feature"),
					Commit:  workerclient.NewOptString("0123456789abcdef0123456789abcdef01234567"),
				}),
				Workspace: workerclient.NewOptGitSourceWorkspace(workerapimodel.GitSourceWorkspace{
					Mode:        workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceModeDirty),
					BaseCommit:  workerclient.NewOptString("0123456789abcdef0123456789abcdef01234567"),
					SnapshotRef: workerclient.NewOptString("refs/discobox/run/run-1"),
				}),
			}),
			SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(
				workerclient.SandboxConfigSourceCodeReferences{
					"/home/user/src/hooks": {
						Kind:     workerclient.GitSourceKindGit,
						Slug:     workerclient.NewOptString("hooks"),
						Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
					},
				}),
		},
	}
}

const originTestCacheKey = "remote/example.com/acme/app"

// originTestRuntime is deliveryTestRuntime with the sandbox's tree in place,
// as provisioning has it by the time its sources' caches are recorded.
func originTestRuntime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	r := deliveryTestRuntime(t)
	if err := os.MkdirAll(r.sandboxRoot(deliveryTestSandboxID), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func makeBareOrigin(t *testing.T, r *DockerSandboxRuntime, slug string) string {
	t.Helper()
	origin := r.sandboxOriginPath(deliveryTestSandboxID, slug)
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return origin
}

// A cache-delivered source's origin route serves the sandbox its own
// namespace of the pool's cache, fetch-only; a source with a bare origin of
// its own is served that, as the sandbox user.
func TestTheOriginRouteServesASourcesNamespaceOfThePoolCache(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	bare := makeBareOrigin(t, r, "hooks")
	if err := r.writeSourceCaches(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/project"))); err != nil {
		t.Fatal(err)
	}

	cached, err := r.originLocation(t.Context(), deliveryTestSandboxID, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Release == nil {
		t.Fatal("a cache was served without being held against collection")
	}
	cached.Release()
	if !cached.FetchOnly || cached.Namespace != deliveryTestSandboxID+"-primary" || !strings.HasPrefix(cached.Path, r.root.PoolGitCache(r.projectID, r.poolID)) {
		t.Fatalf("primary origin = %+v, want its namespace of the pool cache, fetch-only", cached)
	}

	pushed, err := r.originLocation(t.Context(), deliveryTestSandboxID, "hooks", map[string]string{"DISCOBOX_USER_UID": "1000", "DISCOBOX_USER_GID": "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.FetchOnly || pushed.Namespace != "" || pushed.Path != bare || pushed.UID != 1000 || pushed.GID != 1000 {
		t.Fatalf("hooks origin = %+v, want the bare origin %s as the sandbox user", pushed, bare)
	}
	if _, err := r.originLocation(t.Context(), deliveryTestSandboxID, "absent", nil); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("a source the sandbox does not have = %v, want ErrRepositoryNotFound", err)
	}
}

// The record describes the sources the sandbox has now, and a create leaves
// nothing of the live origin an earlier pool served behind it.
func TestSourceCachesAreRewrittenOnEveryCreate(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	legacy := filepath.Join(r.sandboxRoot(deliveryTestSandboxID), legacyLiveOriginsFileName)
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.writeSourceCaches(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/project"))); err != nil {
		t.Fatal(err)
	}
	caches, err := r.readSourceCaches(deliveryTestSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(caches) != 1 || caches["primary"] != originTestCacheKey {
		t.Fatalf("recorded caches = %v, want the primary's alone", caches)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the live origin record survived a create: %v", err)
	}

	if err := r.writeSourceCaches(deliveryTestSandboxID, sandboxSources(linuxPaths, deliveryTestRequest())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.sandboxRoot(deliveryTestSandboxID), sourceCachesFileName)); !os.IsNotExist(err) {
		t.Fatalf("a sandbox with no cache-delivered source kept its record: %v", err)
	}
}

// A source that an earlier pool served live from the developer's Git
// directory gets a bare origin of its own, which a client pushes into; one a
// sandbox cloned from its remote directly has no origin on the pool at all.
func TestOnlyASourceDeliveredBeforeTheCacheHasItsOwnOrigin(t *testing.T) {
	local := workerapimodel.GitSource{LocalDirectory: workerclient.NewOptString("/home/dev/project")}
	pushed := workerapimodel.GitSource{Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush)}
	remote := workerapimodel.GitSource{}
	cached := workerapimodel.GitSource{
		LocalDirectory: workerclient.NewOptString("/home/dev/project"),
		Delivery:       workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryCache),
		CacheKey:       workerclient.NewOptString(originTestCacheKey),
	}
	for name, test := range map[string]struct {
		source                         workerapimodel.GitSource
		ownOrigin, delivered, poolServ bool
	}{
		"a local source once served live": {local, true, false, true},
		"a pushed source":                 {pushed, true, true, true},
		"a remote the sandbox cloned":     {remote, false, false, false},
		"a cache-delivered source":        {cached, false, true, true},
	} {
		if got := gitSourceHasOwnOrigin(test.source); got != test.ownOrigin {
			t.Errorf("%s: has its own origin = %v, want %v", name, got, test.ownOrigin)
		}
		if got := gitSourceClientDelivers(test.source); got != test.delivered {
			t.Errorf("%s: delivered by a client = %v, want %v", name, got, test.delivered)
		}
		if got := poolServesOrigin(test.source); got != test.poolServ {
			t.Errorf("%s: origin served by the pool = %v, want %v", name, got, test.poolServ)
		}
	}
}

// sandbox.json carries how each source is checked out, which the sandbox agent
// reads when it clones the source itself (ADR 0126 §4).
func TestSandboxDocumentCarriesEachSourcesCheckout(t *testing.T) {
	req := originTestRequest("/home/user/src/app")
	primary := req.Config.Source.Value
	primary.UpstreamUrl = workerclient.NewOptString("https://github.com/example/app.git")
	req.Config.Source = workerclient.NewOptGitSource(primary)

	doc := buildSandboxDocument(linuxPaths, "proj_a", deliveryTestSandboxID, "pool_a", "", "", "image", req, nil, nil)
	bySlug := map[string]sandboxconfig.Source{}
	for _, source := range doc.Runtime.Sources {
		bySlug[source.Slug] = source
	}
	got := bySlug["primary"]
	if got.RefName != "feature" || got.RefType != "branch" || got.UpstreamURL != "https://github.com/example/app.git" {
		t.Fatalf("primary = %+v, want branch feature and its remote", got)
	}
	// A cache-delivered source reaches its pool as the discobox remote, and
	// measures its changes against the pool's copy of its branch: origin is
	// the source's own remote.
	if got.PoolRemote != "discobox" || got.UpstreamRef != "refs/remotes/discobox/feature" || !got.AwaitsDelivery {
		t.Fatalf("primary = %+v, want the discobox remote, its branch as the upstream ref, and a delivery awaited", got)
	}
	if hooks := bySlug["hooks"]; hooks.PoolRemote != "" || hooks.UpstreamRef != "refs/remotes/origin/HEAD" {
		t.Fatalf("a source pushed into its own origin = %+v, want origin as its pool remote", hooks)
	}
	want := sandboxconfig.SourceWorkspace{BaseCommit: "0123456789abcdef0123456789abcdef01234567", SnapshotRef: "refs/discobox/run/run-1"}
	if got.Workspace == nil || *got.Workspace != want {
		t.Fatalf("primary workspace = %+v, want %+v", got.Workspace, want)
	}
	if hooks := bySlug["hooks"]; hooks.Workspace != nil || hooks.RefName != "" {
		t.Fatalf("a clean source with no checkout = %+v, want neither", hooks)
	}
}

// A sandbox whose source this pool once served live from the developer's Git
// directory has no bare origin until its container is next rebuilt, which may
// be never. It gets one the first time its origin is asked for, empty until a
// client pushes — and only for a source the pool told the sandbox to fetch
// from it (ADR 26-10-10-545 §8).
func TestAFormerlyLiveOriginGetsItsRepositoryWhenFirstAsked(t *testing.T) {
	requirePOSIXHost(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := originTestRuntime(t)
	user := currentUser()
	env := map[string]string{"DISCOBOX_USER_UID": strconv.FormatInt(*user.UID, 10), "DISCOBOX_USER_GID": strconv.FormatInt(*user.GID, 10)}
	if err := r.recordRuntimeConfig(deliveryTestSandboxID, func(doc *sandboxconfig.RuntimeConfig) {
		doc.Sources = []sandboxconfig.RuntimeSource{
			{Slug: "primary", Target: "/workspace/app", OriginURL: proxyagent.OriginURL(r.projectID, r.poolID, deliveryTestSandboxID, "primary")},
			{Slug: "remote", Target: "/workspace/remote", OriginURL: "https://github.com/example/remote.git"},
		}
	}); err != nil {
		t.Fatal(err)
	}

	location, err := r.originLocation(t.Context(), deliveryTestSandboxID, "primary", env)
	if err != nil {
		t.Fatalf("a formerly live origin: %v", err)
	}
	if location.Path != r.sandboxOriginPath(deliveryTestSandboxID, "primary") || location.FetchOnly || location.Namespace != "" {
		t.Fatalf("origin = %+v, want the sandbox's own bare repository", location)
	}
	if _, err := os.Stat(filepath.Join(location.Path, "HEAD")); err != nil {
		t.Fatalf("the repository was not made: %v", err)
	}
	for _, slug := range []string{"remote", "absent"} {
		if _, err := r.originLocation(t.Context(), deliveryTestSandboxID, slug, env); !errors.Is(err, ErrRepositoryNotFound) {
			t.Fatalf("origin of %q = %v, want ErrRepositoryNotFound", slug, err)
		}
	}
}

// sandbox.json carries the skills sources' directories in the order the
// request gave them, each resolved to the checkout its reference lands in
// (ADR 26-10-14-294 §3).
func TestSandboxDocumentCarriesSkillsSourcesInOrder(t *testing.T) {
	req := originTestRequest("/home/user/src/app")
	tools := workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}
	tools.Slug = workerclient.NewOptString("tools")
	refs := req.Config.SourceCodeReferences.Or(nil)
	if refs == nil {
		refs = workerclient.SandboxConfigSourceCodeReferences{}
	}
	refs["/workspace/skill-sources/tools"] = tools
	req.Config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(refs)
	req.Config.SkillsSources = []workerapimodel.SandboxSkillsSource{
		{Source: "/workspace/skill-sources/tools", Directory: "z-last-alphabetically"},
		{Source: "/workspace/skill-sources/tools", Directory: "."},
	}
	req.Config.Image = workerclient.NewOptString("image")
	req.ResolvedHarnessConfig = workerclient.NewOptResolvedHarnessConfig(workerclient.ResolvedHarnessConfig{})
	if err := validateCreateRequest(deliveryTestSandboxID, req); err != nil {
		t.Fatalf("validate create request: %v", err)
	}

	doc := buildSandboxDocument(linuxPaths, "proj_a", deliveryTestSandboxID, "pool_a", "", "", "image", req, nil, nil)
	want := []sandboxconfig.SkillsSource{
		{Target: "/workspace/skill-sources/tools", Directory: "z-last-alphabetically"},
		{Target: "/workspace/skill-sources/tools", Directory: "."},
	}
	if !reflect.DeepEqual(doc.Runtime.SkillsSources, want) {
		t.Fatalf("skills sources = %+v, want %+v", doc.Runtime.SkillsSources, want)
	}

	req.Config.SkillsSources = []workerapimodel.SandboxSkillsSource{{Source: "/nowhere", Directory: "."}}
	if err := validateCreateRequest(deliveryTestSandboxID, req); err == nil {
		t.Fatal("validate create request naming no source: nil, want a refusal")
	}
}

// sandbox.json carries the repository's features as the request gave them
// (ADR 26-10-16-423 §4).
func TestSandboxDocumentCarriesFeatures(t *testing.T) {
	req := originTestRequest("/home/user/src/app")
	req.Config.Features = workerclient.NewOptSandboxConfigFeatures(workerclient.SandboxConfigFeatures{"brew": {Enabled: false}, "nix": {Enabled: true}})
	req.Config.Image = workerclient.NewOptString("image")
	doc := buildSandboxDocument(linuxPaths, "proj_a", deliveryTestSandboxID, "pool_a", "", "", "image", req, nil, nil)
	want := map[string]sandboxconfig.Feature{"brew": {Enabled: false}, "nix": {Enabled: true}}
	if !reflect.DeepEqual(doc.Runtime.Features, want) {
		t.Fatalf("features = %+v, want %+v", doc.Runtime.Features, want)
	}
	config, _ := sandboxconfig.Effective(doc)
	if !reflect.DeepEqual(config.Features, want) {
		t.Fatalf("sandbox.json features = %+v, want %+v", config.Features, want)
	}
}
