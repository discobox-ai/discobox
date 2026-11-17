package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A create superseded by an archive intent never writes its provider state, so
// the archive that follows has none. It must still ask the sandbox's pool to
// archive what the create made: answering not-found from the empty state
// settles the row archived while the container keeps running on the pool.
func TestPoolProviderArchiveWithoutProviderStateArchivesOnThePool(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	if _, err := runtimeProvider.runtime.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatal(err)
	}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}
	if _, err := provider.Archive(context.Background(), ref, nil); err != nil {
		t.Fatalf("archive without provider state: %v", err)
	}
	if _, err := runtimeProvider.runtime.GetSandbox(context.Background(), "sandbox-1"); !errors.Is(err, sandboxruntime.ErrNotFound) {
		t.Fatalf("sandbox still running on the pool after archive: err = %v, want it archived", err)
	}
}

// Unlike a remove, an archive without state waits on its pool as one with state
// does, and a pool that cannot be reached fails it: it is never reported as
// not-found, which the reconciler settles an archive on as done.
func TestPoolProviderArchiveWithoutProviderStateOnAnUnreachablePoolIsNotNotFound(t *testing.T) {
	hostDown := fmt.Errorf("pool %q: %w", "pool-1", sandbox.ErrPoolNotReachable)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.unreachable = hostDown
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := provider.Archive(ctx, ref, nil); err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("archive without state on an unreachable pool: %v, want it to fail, never not found", err)
	}
}
