package sandboxruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

const deliveryTestSandboxID = "sandbox-1"

// deliveryTestRuntime is a runtime whose state tree lives under a temporary
// directory, so the readiness signal and the source trees can be written and
// inspected without a pool host.
func deliveryTestRuntime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	state := withTestRoot(t)
	return &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a"}
}

// deliveryTestRequest is a sandbox with one push-delivered primary source.
func deliveryTestRequest() *workerapimodel.PoolSandboxCreateRequest {
	return &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: deliveryTestSandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind:           workerclient.GitSourceKindGit,
				Slug:           workerclient.NewOptString("primary"),
				Delivery:       workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
				LocalDirectory: workerclient.NewOptString("/src/primary"),
			}),
		},
	}
}

// markMaterialized writes the marker a source's checkout carries once it has
// been materialized, by the sandbox or, before it did, by a pool.
func markMaterialized(t *testing.T, r *DockerSandboxRuntime, slug string) {
	t.Helper()
	target := r.sandboxSourcePath(deliveryTestSandboxID, slug)
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".git", sandboxconfig.SourceMaterializedMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The sandbox reads a stated intent rather than inferring one: a source the
// client still owes is marked on the document it boots from, and a source the
// sandbox clones from its origin as soon as it boots is not.
func TestSandboxDocumentMarksSourcesAwaitingDelivery(t *testing.T) {
	req := deliveryTestRequest()
	req.Config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(workerclient.SandboxConfigSourceCodeReferences{
		"/src/foo": {
			Kind:           workerclient.GitSourceKindGit,
			Slug:           workerclient.NewOptString("foo"),
			LocalDirectory: workerclient.NewOptString("/src/foo"),
		},
	})

	doc := buildSandboxDocument(linuxPaths, "proj_a", deliveryTestSandboxID, "pool_a", "", "", "image", req, nil, nil)
	byslug := map[string]sandboxconfig.Source{}
	for _, source := range doc.Runtime.Sources {
		byslug[source.Slug] = source
	}
	if !byslug["primary"].AwaitsDelivery {
		t.Fatal("a push-delivered source was not marked as awaiting delivery")
	}
	if byslug["foo"].AwaitsDelivery {
		t.Fatal("a clone-delivered source was marked as awaiting delivery")
	}
	if !sandboxconfig.SourcesAwaitDelivery(doc.Runtime.Sources) {
		t.Fatal("the sandbox does not report that it is waiting on its client")
	}
}

// A sandbox created before the control plane sent the slug holds its secondary
// sources under the slugified reference key. Everything addresses a source by
// slug, so those directories are moved to it — with the work committed in them
// intact, which is the whole reason not to simply materialize afresh.
func TestSourcesMaterializedUnderTheirKeyAreAdoptedByTheirSlug(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	req.Config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(
		workerclient.SandboxConfigSourceCodeReferences{
			"/home/user/src/hooks": {
				Kind:           workerclient.GitSourceKindGit,
				Slug:           workerclient.NewOptString("hooks"),
				LocalDirectory: workerclient.NewOptString("/home/user/src/hooks"),
			},
		})

	markMaterialized(t, runtime, "home-user-src-hooks")
	work := filepath.Join(runtime.sandboxSourcePath(deliveryTestSandboxID, "home-user-src-hooks"), "committed.txt")
	if err := os.WriteFile(work, []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runtime.adoptSourcePaths(context.Background(), deliveryTestSandboxID, sandboxSources(linuxPaths, req)); err != nil {
		t.Fatal(err)
	}

	adopted := runtime.sandboxSourcePath(deliveryTestSandboxID, "hooks")
	if _, err := os.Stat(filepath.Join(adopted, "committed.txt")); err != nil {
		t.Fatalf("the adopted source lost its contents: %v", err)
	}
	if _, err := os.Stat(filepath.Join(adopted, ".git", sandboxconfig.SourceMaterializedMarker)); err != nil {
		t.Fatalf("the adopted source is not materialized, so it would be cloned over: %v", err)
	}
	if _, err := os.Stat(runtime.sandboxSourcePath(deliveryTestSandboxID, "home-user-src-hooks")); !os.IsNotExist(err) {
		t.Fatalf("the old directory is still there: %v", err)
	}
	// The primary's slug is its seed, so it has nothing to adopt and took
	// nothing from the source that did.
	if _, err := os.Stat(runtime.sandboxSourcePath(deliveryTestSandboxID, "primary")); !os.IsNotExist(err) {
		t.Fatalf("the primary source directory was created: %v", err)
	}
}

// A sandbox that already holds the slug's own directory is left alone: the
// materialized source is the one the slug names, and adopting over it would
// swap live work for whatever the old name still holds.
func TestAdoptionLeavesASourceThatAlreadyHasItsSlugAlone(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	req.Config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(
		workerclient.SandboxConfigSourceCodeReferences{
			"/home/user/src/hooks": {
				Kind: workerclient.GitSourceKindGit,
				Slug: workerclient.NewOptString("hooks"),
			},
		})
	markMaterialized(t, runtime, "home-user-src-hooks")
	markMaterialized(t, runtime, "hooks")
	current := filepath.Join(runtime.sandboxSourcePath(deliveryTestSandboxID, "hooks"), "current.txt")
	if err := os.WriteFile(current, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runtime.adoptSourcePaths(context.Background(), deliveryTestSandboxID, sandboxSources(linuxPaths, req)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(current); err != nil {
		t.Fatalf("the source the slug already named was replaced: %v", err)
	}
}
