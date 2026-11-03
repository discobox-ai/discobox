package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// markingPoolManager lists several pools and answers each one's mark from
// markErrs, recording every pool it was asked to mark.
type markingPoolManager struct {
	*fakePoolManager
	pools    []model.Pool
	markErrs map[string]error
	marked   []string
}

func (m *markingPoolManager) ListPoolsForProviderInstance(context.Context, string, string) ([]model.Pool, error) {
	return m.pools, nil
}

func (m *markingPoolManager) SchedulePoolReconciliation(_ context.Context, _, poolID string) error {
	m.marked = append(m.marked, poolID)
	return m.markErrs[poolID]
}

// Initialize run inside a pool's own reconcile has that pool's mark refused as
// a self-mark. That pool is already reconciling, so Initialize succeeds; a
// pool whose mark fails otherwise is reported, and neither stops the pools
// after it from being marked.
func TestInitializeMarksEveryPoolPastAFailedMark(t *testing.T) {
	storeErr := errors.New("database is locked")
	manager := &markingPoolManager{
		fakePoolManager: &fakePoolManager{},
		pools:           []model.Pool{*activePool("pool-1"), *activePool("pool-2"), *activePool("pool-3")},
		markErrs: map[string]error{
			"pool-1": fmt.Errorf("%w (pool project-1/pool-1)", reconcile.ErrSelfMark),
			"pool-2": storeErr,
		},
	}
	provider := New(&testRuntimeProvider{}, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	err := provider.Initialize(context.Background(), instance)
	if !errors.Is(err, storeErr) {
		t.Fatalf("Initialize error = %v, want it to carry %v", err, storeErr)
	}
	if errors.Is(err, reconcile.ErrSelfMark) {
		t.Fatalf("Initialize error = %v, want the self-mark left out", err)
	}
	if want := []string{"pool-1", "pool-2", "pool-3"}; !slices.Equal(manager.marked, want) {
		t.Fatalf("marked %v, want %v", manager.marked, want)
	}

	delete(manager.markErrs, "pool-2")
	manager.marked = nil
	if err := provider.Initialize(context.Background(), instance); err != nil {
		t.Fatalf("Initialize with only a self-mark refused = %v, want nil", err)
	}
}
