package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/store"
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
		t.Fatal("expected schedulable worker lookup")
	}
	if workerStore.sandbox.ID != "sandbox-1" || workerStore.sandbox.ProjectID != "project-1" || workerStore.sandbox.CPUVCPUs != 2 || workerStore.sandbox.MemoryBytes != 1024 || workerStore.sandbox.StorageBytes != 2048 {
		t.Fatalf("schedulable worker sandbox = %#v", workerStore.sandbox)
	}
	if workerStore.sandbox.ProviderInstanceID == nil || *workerStore.sandbox.ProviderInstanceID != "provider-1" {
		t.Fatalf("schedulable worker provider instance = %v, want provider-1", workerStore.sandbox.ProviderInstanceID)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker_id metadata", runtimeSandbox)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
}

func TestWorkerProviderCreateEnsuresCapacityAndWaitsForWorker(t *testing.T) {
	oldTimeout := workerCapacityWaitTimeout
	oldInterval := workerCapacityPollInterval
	workerCapacityWaitTimeout = 50 * time.Millisecond
	workerCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		workerCapacityWaitTimeout = oldTimeout
		workerCapacityPollInterval = oldInterval
	})

	workerStore := &capacityWaitWorkerStore{
		project:  &model.Project{ID: "project-1", TenantID: "tenant-1"},
		provider: &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"},
		worker:   &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, nil, workerStore)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerStore.createdWorkers != 1 {
		t.Fatalf("created workers = %d, want 1", workerStore.createdWorkers)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker-1", runtimeSandbox)
	}
}

func TestWorkerProviderCreateReturnsNoCapacityAfterWait(t *testing.T) {
	oldTimeout := workerCapacityWaitTimeout
	oldInterval := workerCapacityPollInterval
	workerCapacityWaitTimeout = 2 * time.Millisecond
	workerCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		workerCapacityWaitTimeout = oldTimeout
		workerCapacityPollInterval = oldInterval
	})

	workerStore := &capacityWaitWorkerStore{
		project:  &model.Project{ID: "project-1", TenantID: "tenant-1"},
		provider: &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, nil, workerStore)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
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

func (s *recordingWorkerStore) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	return worker, nil
}

func (s *recordingWorkerStore) FindSchedulableWorker(_ context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	s.sandbox = sandbox
	if s.err != nil {
		return nil, s.err
	}
	if s.worker == nil {
		return nil, errors.New("worker is nil")
	}
	return s.worker, nil
}

type capacityWaitWorkerStore struct {
	project        *model.Project
	provider       *model.SandboxProviderInstance
	worker         *model.Worker
	createdWorkers int
}

func (s *capacityWaitWorkerStore) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (s *capacityWaitWorkerStore) GetProject(context.Context, string) (*model.Project, error) {
	return s.project, nil
}

func (s *capacityWaitWorkerStore) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return s.provider, nil
}

func (s *capacityWaitWorkerStore) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	s.createdWorkers++
	return worker, nil
}

func (s *capacityWaitWorkerStore) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	if s.createdWorkers == 0 || s.worker == nil {
		return nil, store.ErrNotFound
	}
	return s.worker, nil
}
