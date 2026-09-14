package model_test

import (
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestPoolHealthRequiresAStatusReportFromThisServerRun(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Second)
	pool := model.Pool{Ready: true, Schedulable: true, LastSeenAt: &now,
		StatusReportedAt: &before, HealthCheckStartedAt: &now}
	if got := pool.Health(now); got != model.PoolHealthUnknown {
		t.Fatalf("previous process report = %s, want unknown", got)
	}
	if got := pool.Health(now.Add(model.PoolHeartbeatTimeout + time.Second)); got != model.PoolHealthOffline {
		t.Fatalf("no report after startup deadline = %s, want offline", got)
	}
	pool.StatusReportedAt = &now
	if got := pool.Health(now); got != model.PoolHealthReady {
		t.Fatalf("fresh report = %s, want ready", got)
	}
	pool.Ready = false
	if got := pool.Health(now); got != model.PoolHealthNotReady {
		t.Fatalf("negative report = %s, want not_ready", got)
	}
	pool.Ready = true
	later := now.Add(model.PoolHeartbeatTimeout + time.Second)
	pool.LastSeenAt = &later // registration is not a health report
	pool.ProvisionProgressAt = &later
	pool.UpdatedAt = later
	if got := pool.Health(later); got != model.PoolHealthOffline {
		t.Fatalf("stale report with fresh unrelated activity = %s, want offline", got)
	}
}

func TestPoolWithoutAStatusReportIsUnknown(t *testing.T) {
	now := time.Now()
	pool := model.Pool{Ready: true, CreatedAt: now, RegisteredAt: &now, LastSeenAt: &now}
	if got := pool.Health(now); got != model.PoolHealthUnknown {
		t.Fatalf("registration without a heartbeat = %s, want unknown", got)
	}
}
