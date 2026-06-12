package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox"
)

func TestDesiredAdditionalWorkersLaunchesReplacementForOnlyDegradedWorker(t *testing.T) {
	workers := []model.Worker{{
		Ready:             true,
		Schedulable:       true,
		Degraded:          true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateActive},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 2, MinHealthy: 1}

	if got := DesiredAdditionalWorkers(workers, cfg); got != 1 {
		t.Fatalf("additional workers = %d, want 1", got)
	}
}

func TestDesiredAdditionalWorkersDoesNotLaunchWhenHealthyMinimumSatisfied(t *testing.T) {
	workers := []model.Worker{{
		Ready:             true,
		Schedulable:       true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateActive},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 2, MinHealthy: 1}

	if got := DesiredAdditionalWorkers(workers, cfg); got != 0 {
		t.Fatalf("additional workers = %d, want 0", got)
	}
}

func TestDesiredAdditionalWorkersHonorsMax(t *testing.T) {
	workers := []model.Worker{{
		Ready:             true,
		Schedulable:       true,
		Degraded:          true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateActive},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}

	if got := DesiredAdditionalWorkers(workers, cfg); got != 0 {
		t.Fatalf("additional workers = %d, want 0", got)
	}
}

func TestNormalizeWorkerPoolConfigKeepsPoolSizeAsMinimumWithReplacementHeadroom(t *testing.T) {
	cfg := NormalizeWorkerPoolConfig(1, 0, 0, 0)
	if cfg.Min != 1 || cfg.Max != 2 || cfg.MinHealthy != 1 {
		t.Fatalf("config = %#v, want min=1 max=2 minHealthy=1", cfg)
	}
}

func TestWorkerProviderCreateClaimsWorkerAndReturnsWorkerID(t *testing.T) {
	createdAt := time.Now().UTC()
	registeredAt := createdAt.Add(time.Second)
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{
			ID:                 "worker-1",
			ProviderInstanceID: "provider-1",
			CreatedAt:          createdAt,
			RegisteredAt:       &registeredAt,
		},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{}, nil, workerStore)

	runtimeSandbox, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		ProviderInstanceID: "provider-1",
		CPUVCPUs:           2,
		MemoryBytes:        1024,
		StorageBytes:       2048,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerStore.sandbox == nil {
		t.Fatal("expected worker claim")
	}
	if workerStore.sandbox.ID != "sandbox-1" || workerStore.sandbox.ProjectID != "project-1" || workerStore.sandbox.CPUVCPUs != 2 || workerStore.sandbox.MemoryBytes != 1024 || workerStore.sandbox.StorageBytes != 2048 {
		t.Fatalf("claimed sandbox = %#v", workerStore.sandbox)
	}
	if workerStore.sandbox.ProviderInstanceID == nil || *workerStore.sandbox.ProviderInstanceID != "provider-1" {
		t.Fatalf("claimed provider instance = %v, want provider-1", workerStore.sandbox.ProviderInstanceID)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker_id metadata", runtimeSandbox)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
}

type recordingWorkerStore struct {
	worker  *model.Worker
	err     error
	sandbox *model.Sandbox
}

func (s *recordingWorkerStore) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (s *recordingWorkerStore) CreateWorkerWithBootstrapToken(context.Context, *model.Worker, *model.WorkerBootstrapToken) error {
	return nil
}

func (s *recordingWorkerStore) ClaimWorker(_ context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	s.sandbox = sandbox
	if s.err != nil {
		return nil, s.err
	}
	if s.worker == nil {
		return nil, errors.New("worker is nil")
	}
	return s.worker, nil
}
