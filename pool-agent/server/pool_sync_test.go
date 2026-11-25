package server

import (
	"context"
	"testing"
	"time"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// syncRecorder is a runtime whose only behavior is SyncKnownPools: it keeps
// the time the set was listed at.
type syncRecorder struct {
	sandboxruntime.Runtime
	listedAt time.Time
}

func (r *syncRecorder) SyncKnownPools(_ context.Context, _ []string, listedAt time.Time) error {
	r.listedAt = listedAt
	return nil
}

// The control plane sends how old its pool set is, not when it read it, so the
// runtime is handed that age back from this clock: a set read a minute ago on
// a control plane whose clock is hours off is still a minute old here.
func TestPoolSyncTakesTheSetsAgeFromThisClock(t *testing.T) {
	identity := Identity{ProjectID: "p1", PoolID: "pool_1"}
	params := workerapi.PoolSyncParams{ProjectId: "p1", PoolId: "pool_1"}

	runtime := &syncRecorder{}
	service := &sandboxService{identity: identity, runtime: runtime}
	before := time.Now()
	req := &workerapimodel.PoolSyncRequest{KnownPoolIds: []string{"pool_1"}, KnownPoolsAgeMs: workerapi.NewOptInt64(time.Minute.Milliseconds())}
	if err := service.PoolSync(t.Context(), req, params); err != nil {
		t.Fatalf("pool sync: %v", err)
	}
	after := time.Now()
	if runtime.listedAt.Before(before.Add(-time.Minute)) || runtime.listedAt.After(after.Add(-time.Minute)) {
		t.Fatalf("set listed at %v, want a minute before the request (between %v and %v)", runtime.listedAt, before.Add(-time.Minute), after.Add(-time.Minute))
	}

	// An older control plane sends no age, and the runtime spares nothing.
	runtime = &syncRecorder{listedAt: time.Now()}
	service = &sandboxService{identity: identity, runtime: runtime}
	if err := service.PoolSync(t.Context(), &workerapimodel.PoolSyncRequest{KnownPoolIds: []string{"pool_1"}}, params); err != nil {
		t.Fatalf("pool sync: %v", err)
	}
	if !runtime.listedAt.IsZero() {
		t.Fatalf("a set with no age was listed at %v, want zero", runtime.listedAt)
	}
}
