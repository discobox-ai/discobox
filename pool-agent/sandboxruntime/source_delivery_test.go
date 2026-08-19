package sandboxruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	workerclient "github.com/obot-platform/discobox/pool-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const deliveryTestSandboxID = "sandbox-1"

// deliveryTestRuntime is a runtime whose state tree lives under a temporary
// directory, so the readiness signal and the source trees can be written and
// inspected without a pool host.
func deliveryTestRuntime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	withTestRoot(t)
	return &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}
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

func readySignalPath(r *DockerSandboxRuntime) string {
	return filepath.Join(r.sandboxConfigRoot(deliveryTestSandboxID), sandboxconfig.SourcesReadyFileName)
}

// The signal says "every source is in place", so it is published only once the
// last one is — and a sandbox still owed a push must not carry a stale one from
// an earlier create.
func TestRefreshSourcesReadyTracksTheSourcesOnDisk(t *testing.T) {
	runtime := deliveryTestRuntime(t)
	req := deliveryTestRequest()
	signal := readySignalPath(runtime)

	// A stale signal from a previous create must be cleared, not left to open
	// the gate on a workspace that is empty again.
	if err := os.MkdirAll(filepath.Dir(signal), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signal, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runtime.refreshSourcesReady(deliveryTestSandboxID, req); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(signal); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the signal cleared while the push is outstanding", signal, err)
	}

	markMaterialized(t, runtime, "primary")
	if err := runtime.refreshSourcesReady(deliveryTestSandboxID, req); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(signal); err != nil {
		t.Fatalf("stat %s = %v, want the signal published once every source is in place", signal, err)
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

	// Once the document records it — which is what the rebuild writes — the
	// same source must stop asking to be rebuilt, or every later create would
	// replace the container again.
	writeTestSandboxDocument(t, runtime.sandboxConfigRoot(deliveryTestSandboxID), &sandboxconfig.ProjectLayer{RunCommand: []string{"./run.sh"}})
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
	writeTestSandboxDocument(t, runtime.sandboxConfigRoot(deliveryTestSandboxID), &sandboxconfig.ProjectLayer{RunCommand: []string{"./run.sh"}})

	rebuild, err := runtime.settleDeliveredSources(context.Background(), deliveryTestSandboxID, req)
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

	doc := buildSandboxDocument("proj_a", deliveryTestSandboxID, "pool_a", "", "image", req, nil, nil)
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

func writeTestSandboxDocument(t *testing.T, configDir string, project *sandboxconfig.ProjectLayer) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&sandboxDocumentFile{Provenance: sandboxconfig.Provenance{Project: project}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, sandboxDocumentName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
