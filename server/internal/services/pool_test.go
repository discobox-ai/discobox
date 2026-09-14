package services

import (
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestPoolAPIHealthMasksStaleFlagsWithoutChangingObservations(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Minute)
	pool := &model.Pool{ID: "pool-1", Ready: true, Schedulable: true,
		StatusReportedAt: &before, HealthCheckStartedAt: &now,
		ResourceLifecycle: model.ResourceLifecycle{State: model.PoolStateActive}}
	view, err := PoolToAPI(pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health) != model.PoolHealthUnknown || view.Ready || view.Schedulable || string(view.State) != model.PoolStateActive {
		t.Fatalf("startup view = %+v", view)
	}
	if !pool.Ready || !pool.Schedulable {
		t.Fatal("projection mutated agent observations")
	}
	pool.StatusReportedAt = &now
	view, err = PoolToAPI(pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health) != model.PoolHealthReady || !view.Ready || !view.Schedulable {
		t.Fatalf("fresh heartbeat view = %+v", view)
	}
	stale := now.Add(-model.PoolHeartbeatTimeout - time.Second)
	pool.StatusReportedAt = &stale
	pool.HealthCheckStartedAt = nil
	view, err = PoolToAPI(pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health) != model.PoolHealthOffline || view.Ready || view.Schedulable {
		t.Fatalf("stale heartbeat view = %+v", view)
	}
}
