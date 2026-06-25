package workerpool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	"github.com/obot-platform/discobox/worker-agent/server"
)

func TestDesiredAdditionalWorkersLaunchesReplacementForOnlyDegradedWorker(t *testing.T) {
	workers := []model.Worker{{
		Ready:             true,
		Schedulable:       true,
		Degraded:          true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateActive},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 2, MinHealthy: 1}

	if got := desiredAdditionalWorkers(workers, cfg); got != 1 {
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

	if got := desiredAdditionalWorkers(workers, cfg); got != 0 {
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

	if got := desiredAdditionalWorkers(workers, cfg); got != 0 {
		t.Fatalf("additional workers = %d, want 0", got)
	}
}

func TestDesiredAdditionalWorkersIgnoresFailedWorkers(t *testing.T) {
	workers := []model.Worker{{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}

	if got := desiredAdditionalWorkers(workers, cfg); got != 1 {
		t.Fatalf("additional workers = %d, want 1", got)
	}
}

func TestEnsureWorkerPoolRepairsWorkersWithFailedJobs(t *testing.T) {
	jobID := "job-1"
	message := "image not found"
	store := &repairingWorkerManager{
		workers: []model.Worker{{
			ID:                 "worker-1",
			ProjectID:          "project-1",
			ProviderInstanceID: "provider-1",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseLaunching,
				LastOperationStatus: model.OperationStatusRunning,
				LastJobID:           &jobID,
			},
		}},
		jobs: map[string]*orchestration.Job{
			jobID: {ID: jobID, Status: orchestration.StatusFailed, Error: &message},
		},
		repairUpdated: true,
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if store.updated == nil {
		t.Fatal("expected stale worker to be updated")
	}
	if store.updated.DesiredState != model.WorkerDesiredStateDeleted || store.updated.LastOperationStatus != model.OperationStatusPending {
		t.Fatalf("updated worker desired/status = %q/%q, want deleted/pending", store.updated.DesiredState, store.updated.LastOperationStatus)
	}
	if store.updated.StatusMessage == nil || *store.updated.StatusMessage != message {
		t.Fatalf("updated worker status message = %v, want %q", store.updated.StatusMessage, message)
	}
	if store.createdWorkers != 1 {
		t.Fatalf("created workers = %d, want replacement", store.createdWorkers)
	}
}

func TestEnsureWorkerPoolSkipsSupersededFailedJobRepair(t *testing.T) {
	jobID := "job-1"
	message := "image not found"
	store := &repairingWorkerManager{
		workers: []model.Worker{{
			ID:                 "worker-1",
			ProjectID:          "project-1",
			ProviderInstanceID: "provider-1",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseLaunching,
				LastOperationStatus: model.OperationStatusRunning,
				LastJobID:           &jobID,
			},
		}},
		jobs: map[string]*orchestration.Job{
			jobID: {ID: jobID, Status: orchestration.StatusFailed, Error: &message},
		},
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if store.updated != nil {
		t.Fatalf("updated worker = %#v, want no superseded repair update", store.updated)
	}
	if store.createdWorkers != 0 {
		t.Fatalf("created workers = %d, want no replacement for superseded repair", store.createdWorkers)
	}
}

func TestEnsureWorkerPoolRepairsExpiredRegisteringWorkers(t *testing.T) {
	oldTimeout := workerRegistrationTimeout
	workerRegistrationTimeout = time.Minute
	t.Cleanup(func() { workerRegistrationTimeout = oldTimeout })

	now := time.Now().UTC()
	store := &repairingWorkerManager{
		workers: []model.Worker{{
			ID:                 "worker-1",
			ProjectID:          "project-1",
			ProviderInstanceID: "provider-1",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseRegistering,
				LastOperationStatus: model.OperationStatusSuccess,
				Generation:          1,
				ObservedGeneration:  1,
			},
			UpdatedAt: now.Add(-2 * time.Minute),
		}},
		repairUpdated: true,
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if store.updated == nil {
		t.Fatal("expected expired registering worker to be updated")
	}
	if store.updated.DesiredState != model.WorkerDesiredStateDeleted || store.updated.LastOperationStatus != model.OperationStatusPending {
		t.Fatalf("updated worker desired/status = %q/%q, want deleted/pending", store.updated.DesiredState, store.updated.LastOperationStatus)
	}
	if store.createdWorkers != 1 {
		t.Fatalf("created workers = %d, want replacement", store.createdWorkers)
	}
}

func TestNormalizeWorkerPoolConfigKeepsPoolSizeAsMinimumWithReplacementHeadroom(t *testing.T) {
	cfg := NormalizeWorkerPoolConfig(1, 0, 0, 0)
	if cfg.Min != 1 || cfg.Max != 2 || cfg.MinHealthy != 1 {
		t.Fatalf("config = %#v, want min=1 max=2 minHealthy=1", cfg)
	}
}

func TestWorkerPoolProviderReconcileRunsInventoryBeforeCapacity(t *testing.T) {
	workerManager := &recordingWorkerManager{}
	workerProvider := &inventoryTestWorkerProvider{}
	pool := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{Max: 1}, workerManager, false)
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := pool.ReconcileWorkerProvider(context.Background(), workerManager, project, provider); err != nil {
		t.Fatalf("reconcile worker provider: %v", err)
	}

	if workerProvider.reconcileCalls != 1 {
		t.Fatalf("inventory reconcile calls = %d, want 1", workerProvider.reconcileCalls)
	}
	if workerProvider.listCallsAtReconcile != 0 {
		t.Fatalf("list calls at inventory reconcile = %d, want 0", workerProvider.listCallsAtReconcile)
	}
	if workerManager.listCalls != 1 {
		t.Fatalf("list calls after reconcile = %d, want 1", workerManager.listCalls)
	}
}

func TestWorkerPoolProviderReconcileDefersCapacityWhenInventorySchedulesWorker(t *testing.T) {
	workerManager := &recordingWorkerManager{}
	workerProvider := &inventoryTestWorkerProvider{pendingWorkerReconcile: true}
	pool := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{Max: 1}, workerManager, false)
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := pool.ReconcileWorkerProvider(context.Background(), workerManager, project, provider); err != nil {
		t.Fatalf("reconcile worker provider: %v", err)
	}

	if workerProvider.reconcileCalls != 1 {
		t.Fatalf("inventory reconcile calls = %d, want 1", workerProvider.reconcileCalls)
	}
	if workerManager.listCalls != 0 {
		t.Fatalf("list calls after deferred reconcile = %d, want 0", workerManager.listCalls)
	}
}

func TestWorkerProviderCreateClaimsWorkerAndReturnsWorkerID(t *testing.T) {
	createdAt := time.Now().UTC()
	registeredAt := createdAt.Add(time.Second)
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{
			ID:                 "worker-1",
			ProviderInstanceID: "provider-1",
			CreatedAt:          createdAt,
			RegisteredAt:       &registeredAt,
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{}, workerManager, false)

	runtimeSandbox, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		ProviderInstanceID: "provider-1",
		CPUVCPUs:           2,
		MemoryBytes:        1024,
		StorageBytes:       2048,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerManager.sandbox == nil {
		t.Fatal("expected schedulable worker lookup")
	}
	if workerManager.sandbox.ID != "sandbox-1" || workerManager.sandbox.ProjectID != "project-1" || workerManager.sandbox.CPUVCPUs != 2 || workerManager.sandbox.MemoryBytes != 1024 || workerManager.sandbox.StorageBytes != 2048 {
		t.Fatalf("schedulable worker sandbox = %#v", workerManager.sandbox)
	}
	if workerManager.sandbox.ProviderInstanceID == nil || *workerManager.sandbox.ProviderInstanceID != "provider-1" {
		t.Fatalf("schedulable worker provider instance = %v, want provider-1", workerManager.sandbox.ProviderInstanceID)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker_id metadata", runtimeSandbox)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
}

func TestWorkerProviderCreateCallsWorkerAgentRuntime(t *testing.T) {
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, workerToken := newWorkerAgentTestAuth(t, "project-1", "worker-1")
	router, _ := server.NewRouter(server.Config{
		Identity:              server.Identity{ProjectID: "project-1", WorkerID: "worker-1"},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	workerAgent := httptest.NewServer(router)
	defer workerAgent.Close()

	driver := &workerHTTPOnlyDriver{baseURL: workerAgent.URL, client: workerAgent.Client(), authTokenProvider: func(context.Context) (string, error) {
		return workerToken, nil
	}}
	baseProvider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: false, Degraded: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerPoolProvider(baseProvider, WorkerPoolConfig{}, workerManager, false)

	runtimeSandbox, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		ProviderInstanceID: "provider-1",
		Image:              sandbox.ImageRef{Name: "alpine:3.20"},
		Env:                map[string]string{"HELLO": "world"},
		Source: &model.GitSource{
			Kind: "git",
			Destination: &model.GitSourceDestination{
				WorkingDirectory: ptrString("/workspace"),
			},
		},
		CPUVCPUs:    2,
		MemoryBytes: 128 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if driver.workerID != "worker-1" {
		t.Fatalf("worker HTTP lease workerID = %q", driver.workerID)
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != "sandbox-1" || runtimeSandbox.Status != sandbox.StatusRunning || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
	created, err := runtime.GetSandbox(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatalf("worker runtime get sandbox: %v", err)
	}
	if created.Image != "alpine:3.20" || created.Env["HELLO"] != "world" {
		t.Fatalf("created sandbox = %#v", created)
	}
}

func TestWorkerProviderCreateWithExistingStateReusesWorker(t *testing.T) {
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: false, Degraded: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{}, workerManager, false)

	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-1"}})
	workerManager.worker = &model.Worker{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true}
	workerManager.findCalls = 0

	runtimeSandbox, nextState, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if workerManager.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerManager.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker-1 metadata", runtimeSandbox)
	}
	if gotWorkerID, err := workerIDFromRuntimeState(nextState); err != nil || gotWorkerID != "worker-1" {
		t.Fatalf("next state worker ID = %q, %v; want worker-1", gotWorkerID, err)
	}
}

func TestWorkerProviderCreateWithExistingStateSkipsScheduling(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-1"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{}, workerManager, false)

	runtimeSandbox, nextState, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerManager.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerManager.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker-1 metadata", runtimeSandbox)
	}
	if gotWorkerID, err := workerIDFromRuntimeState(nextState); err != nil || gotWorkerID != "worker-1" {
		t.Fatalf("next state worker ID = %q, %v; want worker-1", gotWorkerID, err)
	}
}

func TestWorkerProviderCreateWithExistingStateRejectsWrongProviderWorker(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-2", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-2")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{}, workerManager, false)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-2"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
	if workerManager.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerManager.findCalls)
	}
}

func TestWorkerProviderCreateWithUnassignedStateSchedulesWorker(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{}, workerManager, false)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerManager.findCalls != 1 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 1", workerManager.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want scheduled worker-1", runtimeSandbox)
	}
}

func TestVMProviderCreateWorkerTreatsExistingRuntimeStateAsSuccess(t *testing.T) {
	driver := &existingInstanceDriver{instanceID: "instance-1"}
	provider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	worker := &model.Worker{
		ID:           "worker-1",
		RuntimeState: []byte(`{"instanceId":"instance-1"}`),
	}

	err = provider.CreateWorker(context.Background(), &model.Project{ID: "project-1"}, &model.SandboxProviderInstance{ID: "provider-1"}, worker, "bootstrap-token", "control-plane-public-key")
	if err != nil {
		t.Fatalf("launch existing worker: %v", err)
	}
	if driver.createCalls != 0 {
		t.Fatalf("CreateVM calls = %d, want 0 for existing state", driver.createCalls)
	}
	if driver.inspectCalls != 2 {
		t.Fatalf("InspectVM calls = %d, want 2", driver.inspectCalls)
	}
}

func TestStartupReconcileWorkerIncludesRegisteringSuccess(t *testing.T) {
	worker := &model.Worker{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseRegistering,
			LastOperationStatus: model.OperationStatusSuccess,
			Generation:          2,
			ObservedGeneration:  2,
		},
	}
	if !startupReconcileWorker(worker) {
		t.Fatal("startupReconcileWorker() = false, want true for previously launched registering worker")
	}
	worker.ObservedGeneration = 1
	if startupReconcileWorker(worker) {
		t.Fatal("startupReconcileWorker() = true, want false for stale observed generation")
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

	workerManager := &capacityWaitWorkerManager{
		project:  &model.Project{ID: "project-1"},
		provider: &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"},
		worker:   &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, workerManager, false)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerManager.createdWorkers != 1 {
		t.Fatalf("created workers = %d, want 1", workerManager.createdWorkers)
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

	workerManager := &capacityWaitWorkerManager{
		project:  &model.Project{ID: "project-1"},
		provider: &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := NewWorkerPoolProvider(workerProvider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, workerManager, false)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
}

func TestWorkerProviderGetUsesWorkerAPIState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/project/project-1/worker/worker-1/sandboxes/sandbox-1" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&sandbox.Sandbox{
			ID:        "runtime-1",
			SandboxID: "sandbox-1",
			Status:    sandbox.StatusRunning,
			Image:     "image-1",
			CreatedAt: time.Now().UTC(),
			Metadata:  map[string]string{},
			Env:       map[string]string{},
			Ports:     []sandbox.AssignedPort{},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	driver := &workerHTTPOnlyDriver{baseURL: server.URL, client: server.Client(), authToken: "worker-token"}
	baseProvider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	workerManager := &recordingWorkerManager{worker: &model.Worker{ID: "worker-1"}}
	provider := NewWorkerPoolProvider(baseProvider, WorkerPoolConfig{}, workerManager, false)
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"worker_id": "worker-1"}})

	runtimeSandbox, err := provider.Get(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if runtimeSandbox.ID != "runtime-1" || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if driver.inspectCalls != 0 {
		t.Fatalf("InspectVM calls = %d, want 0", driver.inspectCalls)
	}
	if driver.workerID != "worker-1" {
		t.Fatalf("worker HTTP lease workerID = %q", driver.workerID)
	}
	if len(workerManager.workerAgentTokenClaims) != 1 {
		t.Fatalf("worker-agent token claims count = %d, want 1", len(workerManager.workerAgentTokenClaims))
	}
	if claims := workerManager.workerAgentTokenClaims[0]; claims.ProjectID != "project-1" || claims.WorkerID != "worker-1" || claims.SandboxID != "sandbox-1" || !reflect.DeepEqual(claims.Scopes, []string{workeragentauth.ScopeSandboxRead}) {
		t.Fatalf("worker-agent token claims = %#v", claims)
	}
}

func TestWorkerProviderAcquireHTTPClientUsesWorkerIDFromState(t *testing.T) {
	driver := &workerHTTPOnlyDriver{client: http.DefaultClient, baseURL: "https://worker.example", authToken: "worker-token"}
	baseProvider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	workerManager := &recordingWorkerManager{worker: &model.Worker{ID: "worker-1"}}
	provider := NewWorkerPoolProvider(baseProvider, WorkerPoolConfig{}, workerManager, false)
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"worker_id": "worker-1"}})

	lease, err := provider.AcquireHTTPClient(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, []string{workeragentauth.ScopeSandboxRead, workeragentauth.ScopeSandboxWrite})
	if err != nil {
		t.Fatalf("acquire HTTP client: %v", err)
	}
	defer lease.Release()
	if lease.BaseURL != "https://worker.example" || lease.AuthToken != "worker-token" {
		t.Fatalf("lease = %#v", lease)
	}
	token, err := lease.AuthorizationToken(context.Background())
	if err != nil {
		t.Fatalf("lease authorization token: %v", err)
	}
	if token != "worker-token" {
		t.Fatalf("lease token = %q", token)
	}
	if len(workerManager.workerAgentTokenClaims) != 1 {
		t.Fatalf("worker-agent token claims count = %d, want 1", len(workerManager.workerAgentTokenClaims))
	}
	if claims := workerManager.workerAgentTokenClaims[0]; claims.ProjectID != "project-1" || claims.WorkerID != "worker-1" || claims.SandboxID != "sandbox-1" || !reflect.DeepEqual(claims.Scopes, []string{workeragentauth.ScopeSandboxRead, workeragentauth.ScopeSandboxWrite}) {
		t.Fatalf("worker-agent token claims = %#v", claims)
	}
	if driver.workerID != "worker-1" {
		t.Fatalf("worker ID = %q", driver.workerID)
	}
	if driver.inspectCalls != 0 {
		t.Fatalf("InspectVM calls = %d, want 0", driver.inspectCalls)
	}
}

func workerRuntimeState(t *testing.T, runtimeSandbox *sandbox.Sandbox) []byte {
	t.Helper()
	state, err := json.Marshal(runtimeSandbox)
	if err != nil {
		t.Fatalf("marshal worker runtime state: %v", err)
	}
	return state
}

type workerHTTPOnlyDriver struct {
	client            *http.Client
	baseURL           string
	authToken         string
	authTokenProvider func(context.Context) (string, error)
	workerID          string
	inspectCalls      int
}

type existingInstanceDriver struct {
	instanceID   string
	createCalls  int
	inspectCalls int
}

func (d *existingInstanceDriver) CreateVM(context.Context, vm.InstanceSpec) (*vm.Instance, error) {
	d.createCalls++
	return nil, errors.New("CreateVM should not be called for existing state")
}

func (d *existingInstanceDriver) InitializeWorkerProvider(context.Context, *model.SandboxProviderInstance, any) error {
	return nil
}

func (d *existingInstanceDriver) Close() error {
	return nil
}

func (d *existingInstanceDriver) StartVM(context.Context, string) (*vm.Instance, error) {
	return nil, errors.New("StartVM should not be called")
}

func (d *existingInstanceDriver) StopVM(context.Context, string, time.Duration) (*vm.Instance, error) {
	return nil, errors.New("StopVM should not be called")
}

func (d *existingInstanceDriver) DeleteVM(context.Context, string, bool) error {
	return errors.New("DeleteVM should not be called")
}

func (d *existingInstanceDriver) InspectVM(_ context.Context, id string) (*vm.Instance, error) {
	d.inspectCalls++
	if id != d.instanceID {
		return nil, sandbox.ErrNotFound
	}
	return &vm.Instance{ID: id, Status: sandbox.StatusRunning}, nil
}

func (d *workerHTTPOnlyDriver) CreateVM(context.Context, vm.InstanceSpec) (*vm.Instance, error) {
	return nil, errors.New("CreateVM should not be called")
}

func (d *workerHTTPOnlyDriver) InitializeWorkerProvider(context.Context, *model.SandboxProviderInstance, any) error {
	return nil
}

func (d *workerHTTPOnlyDriver) Close() error {
	return nil
}

func (d *workerHTTPOnlyDriver) StartVM(context.Context, string) (*vm.Instance, error) {
	return nil, errors.New("StartVM should not be called")
}

func (d *workerHTTPOnlyDriver) StopVM(context.Context, string, time.Duration) (*vm.Instance, error) {
	return nil, errors.New("StopVM should not be called")
}

func (d *workerHTTPOnlyDriver) DeleteVM(context.Context, string, bool) error {
	return errors.New("DeleteVM should not be called")
}

func (d *workerHTTPOnlyDriver) InspectVM(context.Context, string) (*vm.Instance, error) {
	d.inspectCalls++
	return nil, errors.New("InspectVM should not be called")
}

func (d *workerHTTPOnlyDriver) AcquireWorkerHTTPClient(_ context.Context, workerID string) (*transport.HTTPClientLease, error) {
	d.workerID = workerID
	if d.authTokenProvider != nil {
		return transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(d.client, d.baseURL, d.authTokenProvider, nil), nil
	}
	return transport.NewHTTPClientLeaseWithBaseURLAndAuth(d.client, d.baseURL, d.authToken, nil), nil
}

func newTestWorkerProvider(t *testing.T, projectID, workerID string) *testWorkerProvider {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, workerToken := newWorkerAgentTestAuth(t, projectID, workerID)
	router, _ := server.NewRouter(server.Config{
		Identity:              server.Identity{ProjectID: projectID, WorkerID: workerID},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	workerAgent := httptest.NewServer(router)
	t.Cleanup(workerAgent.Close)
	return &testWorkerProvider{
		baseURL: workerAgent.URL,
		client:  workerAgent.Client(),
		token:   workerToken,
	}
}

type testWorkerProvider struct {
	baseURL string
	client  *http.Client
	token   string
}

func (p *testWorkerProvider) InitializeWorkerProvider(context.Context, *model.SandboxProviderInstance, any) error {
	return nil
}

func (p *testWorkerProvider) CreateWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker, string, string) error {
	return nil
}

func (p *testWorkerProvider) RemoveWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return nil
}

func (p *testWorkerProvider) AcquireWorkerHTTPClient(context.Context, *model.Worker) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(p.client, p.baseURL, func(context.Context) (string, error) {
		return p.token, nil
	}, nil), nil
}

type inventoryTestWorkerProvider struct {
	testWorkerProvider
	pendingWorkerReconcile bool
	reconcileCalls         int
	listCallsAtReconcile   int
}

func (p *inventoryTestWorkerProvider) ReconcileWorkerProviderInventory(_ context.Context, manager any, _ *model.Project, _ *model.SandboxProviderInstance) (bool, error) {
	p.reconcileCalls++
	if recording, ok := manager.(*recordingWorkerManager); ok {
		p.listCallsAtReconcile = recording.listCalls
	}
	return p.pendingWorkerReconcile, nil
}

type recordingWorkerManager struct {
	worker                  *model.Worker
	workersByID             map[string]*model.Worker
	err                     error
	sandbox                 *model.Sandbox
	findCalls               int
	listCalls               int
	workerAgentTokenClaims  []workeragentauth.TokenClaims
	sandboxAgentTokenClaims []workeragentauth.TokenClaims
}

func (s *recordingWorkerManager) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	s.listCalls++
	return nil, nil
}

func (s *recordingWorkerManager) GetWorker(_ context.Context, workerID string) (*model.Worker, error) {
	if s.workersByID != nil {
		if worker := s.workersByID[workerID]; worker != nil {
			return worker, nil
		}
	}
	if s.worker != nil && s.worker.ID == workerID {
		return s.worker, nil
	}
	return nil, apperrors.ErrNotFound
}

func (s *recordingWorkerManager) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	return worker, nil
}

func (s *recordingWorkerManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	return nil
}

func (s *recordingWorkerManager) EnsureWorkerAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (s *recordingWorkerManager) CreateWorkerAgentToken(_ context.Context, claims workeragentauth.TokenClaims) (string, error) {
	s.workerAgentTokenClaims = append(s.workerAgentTokenClaims, claims)
	return "worker-token", nil
}

func (s *recordingWorkerManager) CreateSandboxAgentToken(_ context.Context, claims workeragentauth.TokenClaims) (string, error) {
	s.sandboxAgentTokenClaims = append(s.sandboxAgentTokenClaims, claims)
	return "sandbox-agent-token", nil
}

func (s *recordingWorkerManager) FindSchedulableWorker(_ context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	s.findCalls++
	s.sandbox = sandbox
	if s.err != nil {
		return nil, s.err
	}
	if s.worker == nil {
		return nil, errors.New("worker is nil")
	}
	return s.worker, nil
}

func (s *recordingWorkerManager) ScheduleWorkerProviderReconciliation(context.Context, string, string) error {
	return nil
}

func (s *recordingWorkerManager) ScheduleWorkerReconciliation(context.Context, string) error {
	return nil
}

type capacityWaitWorkerManager struct {
	project        *model.Project
	provider       *model.SandboxProviderInstance
	worker         *model.Worker
	createdWorkers int
}

func (s *capacityWaitWorkerManager) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (s *capacityWaitWorkerManager) GetWorker(context.Context, string) (*model.Worker, error) {
	if s.worker == nil {
		return nil, apperrors.ErrNotFound
	}
	return s.worker, nil
}

func (s *capacityWaitWorkerManager) GetProject(context.Context, string) (*model.Project, error) {
	return s.project, nil
}

func (s *capacityWaitWorkerManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return s.provider, nil
}

func (s *capacityWaitWorkerManager) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	s.createdWorkers++
	return worker, nil
}

func (s *capacityWaitWorkerManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	return nil
}

func (s *capacityWaitWorkerManager) EnsureWorkerAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (s *capacityWaitWorkerManager) CreateWorkerAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "worker-token", nil
}

func (s *capacityWaitWorkerManager) CreateSandboxAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "sandbox-agent-token", nil
}

func (s *capacityWaitWorkerManager) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	if s.createdWorkers == 0 || s.worker == nil {
		return nil, apperrors.ErrNotFound
	}
	return s.worker, nil
}

func (s *capacityWaitWorkerManager) ScheduleWorkerProviderReconciliation(context.Context, string, string) error {
	return nil
}

func (s *capacityWaitWorkerManager) ScheduleWorkerReconciliation(context.Context, string) error {
	return nil
}

type repairingWorkerManager struct {
	workers        []model.Worker
	jobs           map[string]*orchestration.Job
	updated        *model.Worker
	repairUpdated  bool
	createdWorkers int
}

func (s *repairingWorkerManager) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return s.workers, nil
}

func (s *repairingWorkerManager) GetWorker(_ context.Context, workerID string) (*model.Worker, error) {
	for i := range s.workers {
		if s.workers[i].ID == workerID {
			return &s.workers[i], nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (s *repairingWorkerManager) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	s.createdWorkers++
	return worker, nil
}

func (s *repairingWorkerManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	return nil
}

func (s *repairingWorkerManager) EnsureWorkerAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (s *repairingWorkerManager) CreateWorkerAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "worker-token", nil
}

func (s *repairingWorkerManager) CreateSandboxAgentToken(context.Context, workeragentauth.TokenClaims) (string, error) {
	return "sandbox-agent-token", nil
}

func (s *repairingWorkerManager) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	return nil, apperrors.ErrNotFound
}

func (s *repairingWorkerManager) ScheduleWorkerProviderReconciliation(context.Context, string, string) error {
	return nil
}

func (s *repairingWorkerManager) ScheduleWorkerReconciliation(context.Context, string) error {
	return nil
}

func (s *repairingWorkerManager) GetJob(_ context.Context, id string) (*orchestration.Job, error) {
	job := s.jobs[id]
	if job == nil {
		return nil, orchestration.ErrJobNotFound
	}
	return job, nil
}

func (s *repairingWorkerManager) DeleteWorkerForFailedJob(_ context.Context, workerID string, generation int64, jobID string, message string) (bool, error) {
	if !s.repairUpdated {
		return false, nil
	}
	var copied model.Worker
	for _, worker := range s.workers {
		if worker.ID == workerID && worker.Generation == generation && worker.LastJobID != nil && *worker.LastJobID == jobID {
			copied = worker
			break
		}
	}
	if copied.ID == "" {
		return false, nil
	}
	copied.IncrementGeneration()
	copied.BeginOperation(model.WorkerDeleteOperation, nil)
	copied.StatusMessage = &message
	s.updated = &copied
	return true, nil
}

func (s *repairingWorkerManager) DeleteWorkerForExpiredRegistration(_ context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
	if !s.repairUpdated {
		return false, nil
	}
	var copied model.Worker
	for _, worker := range s.workers {
		if worker.ID == workerID &&
			worker.Generation == generation &&
			worker.Phase == model.WorkerPhaseRegistering &&
			worker.LastOperationStatus == model.OperationStatusSuccess &&
			worker.RegisteredAt == nil &&
			worker.LastSeenAt == nil &&
			worker.UpdatedAt.Before(cutoff) {
			copied = worker
			break
		}
	}
	if copied.ID == "" {
		return false, nil
	}
	copied.IncrementGeneration()
	copied.BeginOperation(model.WorkerDeleteOperation, nil)
	copied.StatusMessage = &message
	s.updated = &copied
	return true, nil
}
