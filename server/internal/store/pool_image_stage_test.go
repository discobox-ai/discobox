package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Staging is a condition and never a health state: a pool whose images are not
// staged is active, healthy and schedulable, because a sandbox that wants an
// image the host does not have pulls it then.
//
// This is the write that has to honor that. It runs while a pool's own
// reconcile is writing the same row, so it touches its own three columns and
// nothing else — and the columns it must not touch are the ones scheduling
// reads.
func TestRecordPoolImageStageLeavesPoolHealthAlone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	// A pool that is up and taking work.
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 4, 1<<30, 1<<30, nil); err != nil {
		t.Fatalf("update pool status: %v", err)
	}

	stage, err := json.Marshal(model.PoolImageStage{State: model.PoolImageStateFailed, Total: 4, Error: "unauthorized"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPoolImageStage(ctx, "pool-1", stage, false, time.Now().UTC()); err != nil {
		t.Fatalf("record image stage: %v", err)
	}

	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	// The failure is recorded...
	if pool.ImagesStaged {
		t.Fatal("a failed stage reported the images as staged")
	}
	var recorded model.PoolImageStage
	if err := json.Unmarshal(pool.ImageStage, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.State != model.PoolImageStateFailed || recorded.Error != "unauthorized" {
		t.Fatalf("recorded = %+v, want the failure and its reason", recorded)
	}
	// ...and the pool is still a pool that can take work.
	if !pool.Ready || !pool.Schedulable || pool.Degraded {
		t.Fatalf("image staging changed pool health: ready=%v schedulable=%v degraded=%v",
			pool.Ready, pool.Schedulable, pool.Degraded)
	}
	if pool.AvailableCPUVCPUs != 4 {
		t.Fatalf("image staging stomped agent-reported capacity: %v", pool.AvailableCPUVCPUs)
	}
}

// And a successful stage sets the flag a client waits on, without touching
// anything else either.
func TestRecordPoolImageStageMarksStaged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 2, 1<<30, 1<<30, nil); err != nil {
		t.Fatalf("update pool status: %v", err)
	}

	stage, err := json.Marshal(model.PoolImageStage{State: model.PoolImageStateReady, Done: 4, Total: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPoolImageStage(ctx, "pool-1", stage, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if !pool.ImagesStaged {
		t.Fatal("a successful stage did not mark the pool staged")
	}
	if pool.ImageStagedAt == nil {
		t.Fatal("no timestamp recorded")
	}
	if !pool.Ready || !pool.Schedulable {
		t.Fatal("image staging changed pool health")
	}
}
