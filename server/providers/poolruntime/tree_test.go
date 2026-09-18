package poolruntime

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// newTreeRuntimeProvider serves a pool agent that holds one sandbox's tree, so
// a test can ask for it without a container anywhere.
func newTreeRuntimeProvider(t *testing.T, sandboxID string, scopes ...string) *testRuntimeProvider {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	if err := runtime.ImportTree(context.Background(), sandboxID, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", scopes...)
	router, _ := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: runtime}
}

// A sandbox whose create failed before its agent reported has no runtime state
// naming a pool, and is exactly the sandbox somebody wants to export: broken
// where it is, with a tree on the pool its row still names. Reading the pool out
// of runtime state alone answered 404 for it.
func TestExportTreeFallsBackToTheNamedPoolWhenThereIsNoRuntimeState(t *testing.T) {
	runtimeProvider := newTreeRuntimeProvider(t, "sandbox-1", poolagentserver.ScopeSandboxRead)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	stream, err := provider.ExportTree(context.Background(),
		sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, "pool-1", sandbox.ImageRef{}, nil)
	if err != nil {
		t.Fatalf("export with no runtime state: %v", err)
	}
	defer stream.Close()
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("read the exported tree: %v", err)
	}
}

// With no runtime state and no pool named either, there is nothing to address
// and the original answer stands.
func TestExportTreeWithoutAPoolStillReportsNotFound(t *testing.T) {
	runtimeProvider := newTreeRuntimeProvider(t, "sandbox-1", poolagentserver.ScopeSandboxRead)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	if _, err := provider.ExportTree(context.Background(),
		sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, "", sandbox.ImageRef{}, nil); err == nil {
		t.Fatal("an export with neither runtime state nor a pool was accepted")
	}
}

// A failed hop to the pool agent must not hand the caller the agent's address.
// http.Client wraps every transport failure in a *url.Error carrying the whole
// target — the agent's internal host and port, the project, the pool, and the
// sandbox id an import had just allocated — and returning it as it comes
// described a machine the caller cannot see while burying the one word that
// says what went wrong.
func TestPoolAgentTransportErrorDropsTheAgentsAddress(t *testing.T) {
	underlying := io.ErrUnexpectedEOF
	err := poolAgentTransportError("send the sandbox tree to", "pool-1", &url.Error{
		Op:  "Put",
		URL: "http://127.0.0.1:32791/api/project/proj_secret/pool/pool-1/sandboxes/sbx_secret/tree",
		Err: underlying,
	})
	for _, leaked := range []string{"127.0.0.1", "32791", "proj_secret", "sbx_secret", "http://"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the error carries %q: %v", leaked, err)
		}
	}
	// The cause is what the caller can act on, so it survives both as text and
	// for errors.Is.
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("the cause was dropped: %v", err)
	}
	if !errors.Is(err, underlying) {
		t.Errorf("errors.Is no longer reaches the cause: %v", err)
	}
	// An error that is not a *url.Error passes through unchanged but still
	// names the pool.
	plain := poolAgentTransportError("read the sandbox tree from", "pool-2", errors.New("boom"))
	if !strings.Contains(plain.Error(), "pool-2") || !strings.Contains(plain.Error(), "boom") {
		t.Errorf("plain error = %v", plain)
	}
}

// imageRecordingRuntime notes the image the pool agent was asked to read a
// tree with.
type imageRecordingRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	got *sandboxruntime.TreeImage
}

func (r imageRecordingRuntime) ExportTree(ctx context.Context, sandboxID string, image sandboxruntime.TreeImage) (io.ReadCloser, error) {
	*r.got = image
	return r.MemorySandboxRuntime.ExportTree(ctx, sandboxID, image)
}

// The pin crosses the hop to the pool agent intact: its sandbox agent reads the
// tree, so the image it runs is the one the export needs (ADR 0129 §1). The
// query names here are mirrored rather than imported, and this is what holds
// the two ends together.
func TestExportTreeSendsThePinToThePool(t *testing.T) {
	memory := sandboxruntime.NewMemorySandboxRuntime()
	if err := memory.ImportTree(context.Background(), "sandbox-1", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var got sandboxruntime.TreeImage
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", poolagentserver.ScopeSandboxRead)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               imageRecordingRuntime{MemorySandboxRuntime: memory, got: &got},
		ControlPlanePublicKey: controlPlaneKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	runtimeProvider := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: memory}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, &fakePoolManager{pool: activePool("pool-1"), schedulable: true})

	pin := sandbox.ImageRef{Name: "registry.example/harness:v1", Digest: "sha256:abc"}
	stream, err := provider.ExportTree(context.Background(),
		sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, "pool-1", pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if got.Name != pin.Name || got.Digest != pin.Digest {
		t.Fatalf("pool agent read with %+v, want %+v", got, pin)
	}
}
