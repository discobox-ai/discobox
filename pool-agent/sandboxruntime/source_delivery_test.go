package sandboxruntime

import (
	"context"
	"encoding/json"
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

// markMaterialized writes the marker materializeGitSource leaves behind when a
// source has been checked out.
func markMaterialized(t *testing.T, r *DockerSandboxRuntime, slug string) {
	t.Helper()
	target := r.sandboxSourcePath(deliveryTestSandboxID, slug)
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitMaterializedMarkerPath(target), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A source is delivered in the runtime-config document once it is in place,
// and not before: a sandbox still owed a push keeps waiting (ADR 0055).
func TestSourcesAreDeliveredOnceTheyAreOnDisk(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	delivered := func() bool {
		return sandboxconfig.RuntimeConfig{Sources: runtimeSources(sandboxSources(linuxPaths, req), runtime.sourceMaterialized(deliveryTestSandboxID))}.SourcesDelivered()
	}
	if delivered() {
		t.Fatal("a source still owed a push is delivered")
	}
	markMaterialized(t, runtime, "primary")
	if !delivered() {
		t.Fatal("a source in place is not delivered")
	}
}

// The project layer inside a delivered source cannot be read when the container
// is created, so its arrival is what the resume has to notice.
func TestProjectLayerChangedSeesADeliveredProjectLayer(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()

	// Nothing delivered and nothing recorded: the ordinary case, and no reason
	// to rebuild anything.
	changed, err := runtime.projectLayerChanged(deliveryTestSandboxID, req)
	if err != nil {
		t.Fatalf("projectLayerChanged: %v", err)
	}
	if changed {
		t.Fatal("a source that declares nothing was reported as changed")
	}

	writeTestProjectLayer(t, runtime.sandboxSourcePath(deliveryTestSandboxID, "primary"), `{"runCommand":["./run.sh"]}`)
	changed, err = runtime.projectLayerChanged(deliveryTestSandboxID, req)
	if err != nil {
		t.Fatalf("projectLayerChanged: %v", err)
	}
	if !changed {
		t.Fatal("a delivered project layer was not noticed")
	}

	// Once the pool records it — which is what the rebuild's bootstrap writes —
	// the same source must stop asking to be rebuilt, or every later create
	// would replace the container again.
	writeTestProjectLayerRecord(t, runtime, &sandboxconfig.ProjectLayer{RunCommand: []string{"./run.sh"}})
	changed, err = runtime.projectLayerChanged(deliveryTestSandboxID, req)
	if err != nil {
		t.Fatalf("projectLayerChanged: %v", err)
	}
	if changed {
		t.Fatal("a project layer already baked into the document asked for another rebuild")
	}
}

// settleDeliveredSources is the resume's whole decision: it must do nothing for
// a sandbox that owes nothing, so a retried or repeated create cannot rebuild
// the container in a loop.
func TestSettleDeliveredSourcesIsANoOpOnceEverythingIsInPlace(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	markMaterialized(t, runtime, "primary")
	writeTestProjectLayer(t, runtime.sandboxSourcePath(deliveryTestSandboxID, "primary"), `{"runCommand":["./run.sh"]}`)
	writeTestProjectLayerRecord(t, runtime, &sandboxconfig.ProjectLayer{RunCommand: []string{"./run.sh"}})

	rebuild, err := runtime.settleDeliveredSources(context.Background(), &Sandbox{SandboxID: deliveryTestSandboxID, Status: StatusStopped}, req)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rebuild {
		t.Fatal("a settled sandbox asked for a rebuild")
	}
}

// The sandbox reads a stated intent rather than inferring one: a source the
// client still owes is marked on the document it boots from, and a source that
// was materialized before the container existed is not.
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

func writeTestProjectLayer(t *testing.T, sourceDir, body string) {
	t.Helper()
	dir := filepath.Join(sourceDir, ".discobox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestProjectLayerRecord(t *testing.T, runtime *DockerSandboxRuntime, project *sandboxconfig.ProjectLayer) {
	t.Helper()
	if err := runtime.writeProjectLayerRecord(deliveryTestSandboxID, project); err != nil {
		t.Fatal(err)
	}
}

// The pool decides whether a delivered project layer changed from its own
// record, never from sandbox.json, which the sandbox can write once it boots
// (ADR 26-10-08-127).
func TestProjectLayerChangedIgnoresWhatSandboxJSONSays(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	writeTestProjectLayer(t, runtime.sandboxSourcePath(deliveryTestSandboxID, "primary"), `{"runCommand":["./run.sh"]}`)
	configDir := runtime.sandboxConfigRoot(deliveryTestSandboxID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&sandboxDocumentFile{Provenance: sandboxconfig.Provenance{Project: &sandboxconfig.ProjectLayer{RunCommand: []string{"./run.sh"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, sandboxDocumentName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := runtime.projectLayerChanged(deliveryTestSandboxID, req)
	if err != nil || !changed {
		t.Fatalf("projectLayerChanged = %v, %v; want the pool's own record to decide", changed, err)
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
	if !gitSourceMaterialized(adopted) {
		t.Fatal("the adopted source is not materialized, so it would be cloned over")
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
