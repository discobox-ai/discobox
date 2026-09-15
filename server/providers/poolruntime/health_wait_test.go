package poolruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// Uses the real health rule with the in-memory control plane.
type healthPoolManager struct{ *fakePoolManager }

func (m healthPoolManager) SchedulablePoolForSandbox(ctx context.Context, sb *model.Sandbox) (*model.Pool, error) {
	pool, err := m.GetPool(ctx, sb.ProjectID, sb.PoolID)
	if err != nil {
		return nil, err
	}
	if !pool.IsReady() || !pool.Schedulable {
		return nil, apperrors.ErrNotFound
	}
	return pool, nil
}

func TestCreateWaitsForPostRestartHeartbeat(t *testing.T) {
	oldTimeout, oldPoll := poolCapacityWaitTimeout, poolCapacityPollInterval
	poolCapacityWaitTimeout, poolCapacityPollInterval = time.Second, time.Millisecond
	t.Cleanup(func() { poolCapacityWaitTimeout, poolCapacityPollInterval = oldTimeout, oldPoll })
	now := time.Now()
	old := now.Add(-time.Second)
	pool := activePool("pool-1")
	pool.StatusReportedAt, pool.HealthCheckStartedAt = &old, &now
	pool.ReconciledAt = &old
	pool.RecordFailure(model.PoolStateOffline, "pool agent has not reported since the previous server run", "")
	pool.UpdatedAt = now // unrelated writes cannot renew the previous verdict
	manager := healthPoolManager{&fakePoolManager{pool: pool}}
	provider := New(newTestRuntimeProvider(t, "project-1", "pool-1"), sandbox.ProviderDefinition{Name: "test"}, manager)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("create finished before a fresh heartbeat: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	manager.mu.Lock()
	reported := time.Now()
	manager.pool.StatusReportedAt = &reported
	manager.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("create after heartbeat: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("create did not resume after heartbeat")
	}
}

func TestUnknownPoolTimeoutExplainsTheAgentWait(t *testing.T) {
	oldTimeout, oldPoll := poolCapacityWaitTimeout, poolCapacityPollInterval
	poolCapacityWaitTimeout, poolCapacityPollInterval = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { poolCapacityWaitTimeout, poolCapacityPollInterval = oldTimeout, oldPoll })
	manager := healthPoolManager{&fakePoolManager{pool: activePool("pool-1")}}
	provider := New(newTestRuntimeProvider(t, "project-1", "pool-1"), sandbox.ProviderDefinition{Name: "test"}, manager)
	_, err := provider.schedulablePool(context.Background(), &model.Sandbox{ProjectID: "project-1", PoolID: "pool-1"})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for the pool agent to become ready") {
		t.Fatalf("timeout = %v", err)
	}
}

func TestStartupWaitSurfacesOnlyCurrentRuntimeFailures(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Minute)
	pool := activePool("pool-1")
	pool.HealthCheckStartedAt = &now
	pool.RecordFailure(model.PoolStateOffline, "runtime could not start", "")
	for _, at := range []*time.Time{nil, &old} {
		pool.ReconciledAt = at
		if err := settledFailure(pool); err != nil {
			t.Fatalf("previous-run failure ended startup wait: %v", err)
		}
	}
	pool.ReconciledAt = &now
	if err := settledFailure(pool); err == nil || !strings.Contains(err.Error(), "runtime could not start") {
		t.Fatalf("current runtime failure = %v", err)
	}
}
