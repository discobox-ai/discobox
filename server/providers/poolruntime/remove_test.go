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

// A create that failed or went unanswered after the agent made the container
// leaves the sandbox with no provider state. Removing it must still ask the
// sandbox's pool to delete it: answering not-found from the empty state lets
// the control plane drop the row while the container stays on the pool.
func TestPoolProviderRemoveWithoutProviderStateDeletesOnThePool(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	if _, err := runtimeProvider.runtime.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatal(err)
	}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}
	if _, err := provider.Remove(context.Background(), ref, nil); err != nil {
		t.Fatalf("remove without provider state: %v", err)
	}
	if _, err := runtimeProvider.runtime.GetSandbox(context.Background(), "sandbox-1"); !errors.Is(err, sandboxruntime.ErrNotFound) {
		t.Fatalf("sandbox on the pool after remove: err = %v, want it gone", err)
	}
}

// A sandbox the pool never made is asked of its pool too, and the answer that
// nothing is there lets its row go.
func TestPoolProviderRemoveWithoutProviderStateOfASandboxThePoolNeverMade(t *testing.T) {
	runtimeProvider := newClearCacheRuntimeProvider(t, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}
	if _, err := provider.Remove(context.Background(), ref, nil); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("remove of a sandbox the pool never made: %v, want nil or not found", err)
	}
}

// A sandbox with no state is the one most likely to sit on a pool that never
// came up, which cannot be deleted while it holds sandboxes. Its delete does
// not wait on that pool: unreachable now, the answer is not-found and the row
// goes, leaving what the pool made to its reaper. A sandbox with state still
// waits for its pool.
func TestPoolProviderRemoveWithoutProviderStateDoesNotWaitOnAnUnreachablePool(t *testing.T) {
	hostDown := fmt.Errorf("pool %q: %w", "pool-1", sandbox.ErrPoolNotReachable)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.unreachable = hostDown
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}

	if _, err := provider.Remove(context.Background(), ref, nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("remove without state on an unreachable pool: %v, want not found", err)
	}
	if runtimeProvider.acquireCalls != 1 || manager.scheduledReconciles != 0 {
		t.Fatalf("acquires = %d, reconciles = %d; want one acquire and no reconcile of the pool", runtimeProvider.acquireCalls, manager.scheduledReconciles)
	}

	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := provider.Remove(ctx, ref, state); err == nil || errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("remove with state on an unreachable pool: %v, want it to fail and be retried", err)
	}
}
