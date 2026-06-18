package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/obot-platform/discobox/apperrors"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
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

func TestDesiredAdditionalWorkersIgnoresFailedWorkers(t *testing.T) {
	workers := []model.Worker{{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}

	if got := DesiredAdditionalWorkers(workers, cfg); got != 1 {
		t.Fatalf("additional workers = %d, want 1", got)
	}
}

func TestEnsureWorkerPoolRepairsWorkersWithFailedJobs(t *testing.T) {
	jobID := "job-1"
	message := "image not found"
	store := &repairingWorkerStore{
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

	if err := EnsureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
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
	store := &repairingWorkerStore{
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

	if err := EnsureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
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
	store := &repairingWorkerStore{
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

	if err := EnsureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
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

func TestWorkerProviderCreateCallsWorkerAgentRuntime(t *testing.T) {
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	router, _ := server.NewRouter(server.Config{
		Identity: server.Identity{ProjectID: "project-1", WorkerID: "worker-1"},
		Runtime:  runtime,
		AuthTokens: []string{
			"worker-token",
		},
	})
	workerAgent := httptest.NewServer(router)
	defer workerAgent.Close()

	driver := &workerHTTPOnlyDriver{baseURL: workerAgent.URL, client: workerAgent.Client(), authToken: "worker-token"}
	baseProvider, err := New(Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: false, Degraded: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerProvider(baseProvider, WorkerPoolConfig{}, nil, workerStore)

	runtimeSandbox, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		ProviderInstanceID: "provider-1",
		Image:              sandbox.ImageRef{Name: "alpine:3.20"},
		Env:                map[string]string{"HELLO": "world"},
		WorkingDirectory:   "/workspace",
		CPUVCPUs:           2,
		MemoryBytes:        128 * 1024 * 1024,
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
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: false, Degraded: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{}, nil, workerStore)

	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-1"}})
	workerStore.worker = &model.Worker{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true}
	workerStore.findCalls = 0

	runtimeSandbox, nextState, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if workerStore.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerStore.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker-1 metadata", runtimeSandbox)
	}
	if gotWorkerID, err := workerIDFromRuntimeState(nextState); err != nil || gotWorkerID != "worker-1" {
		t.Fatalf("next state worker ID = %q, %v; want worker-1", gotWorkerID, err)
	}
}

func TestWorkerProviderCreateWithExistingStateSkipsSchedulingWithoutBaseProvider(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-1"}})
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{}, nil, workerStore)

	runtimeSandbox, nextState, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerStore.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerStore.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want worker-1 metadata", runtimeSandbox)
	}
	if string(nextState) != string(state) {
		t.Fatalf("state changed: %s != %s", nextState, state)
	}
}

func TestWorkerProviderCreateWithExistingStateRejectsWrongProviderWorker(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-2", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{}, nil, workerStore)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-2"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
	if workerStore.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerStore.findCalls)
	}
}

func TestWorkerProviderCreateWithUnassignedStateSchedulesWorker(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "warm-worker", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerStore := &recordingWorkerStore{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{}, nil, workerStore)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if workerStore.findCalls != 1 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 1", workerStore.findCalls)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v, want scheduled worker-1", runtimeSandbox)
	}
}

func TestLaunchWorkerTreatsExistingRuntimeStateAsSuccess(t *testing.T) {
	driver := &existingInstanceDriver{instanceID: "instance-1"}
	factory := func(_ context.Context, cfg Config) (sandbox.Provider, error) {
		return New(Config{
			Driver:          driver,
			ControlPlaneURL: cfg.ControlPlaneURL,
			DefaultImage:    cfg.DefaultImage,
			AgentPort:       cfg.AgentPort,
			Bootstrap:       cfg.Bootstrap,
			Metadata:        cfg.Metadata,
		})
	}
	worker := &model.Worker{
		ID:           "worker-1",
		RuntimeState: []byte(`{"instanceId":"instance-1"}`),
	}

	err := LaunchWorker(context.Background(), &model.Project{ID: "project-1"}, &model.SandboxProviderInstance{ID: "provider-1"}, worker, "bootstrap-token", LaunchWorkerConfig{Factory: factory})
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

func TestShouldRecreateWorkerRuntime(t *testing.T) {
	for _, tt := range []struct {
		name         string
		runtime      *sandbox.Sandbox
		desiredImage string
		want         bool
	}{
		{name: "missing runtime", want: true},
		{name: "stopped runtime", runtime: &sandbox.Sandbox{Status: sandbox.StatusStopped, Image: "image-1"}, desiredImage: "image-1", want: true},
		{name: "failed runtime", runtime: &sandbox.Sandbox{Status: sandbox.StatusFailed, Image: "image-1"}, desiredImage: "image-1", want: true},
		{name: "changed image", runtime: &sandbox.Sandbox{Status: sandbox.StatusRunning, Image: "image-1"}, desiredImage: "image-2", want: true},
		{name: "running desired image", runtime: &sandbox.Sandbox{Status: sandbox.StatusRunning, Image: "image-1"}, desiredImage: "image-1", want: false},
		{name: "running without desired image", runtime: &sandbox.Sandbox{Status: sandbox.StatusRunning, Image: "image-1"}, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRecreateWorkerRuntime(tt.runtime, tt.desiredImage); got != tt.want {
				t.Fatalf("shouldRecreateWorkerRuntime() = %t, want %t", got, tt.want)
			}
		})
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

	workerStore := &capacityWaitWorkerStore{
		project:  &model.Project{ID: "project-1"},
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
		project:  &model.Project{ID: "project-1"},
		provider: &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"},
	}
	provider := NewWorkerProvider(nil, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, nil, workerStore)

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
	baseProvider, err := New(Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	provider := NewWorkerProvider(baseProvider, WorkerPoolConfig{}, nil, nil)
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
}

func TestWorkerProviderAcquireHTTPClientUsesWorkerIDFromState(t *testing.T) {
	driver := &workerHTTPOnlyDriver{client: http.DefaultClient, baseURL: "https://worker.example", authToken: "worker-token"}
	baseProvider, err := New(Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	provider := NewWorkerProvider(baseProvider, WorkerPoolConfig{}, nil, nil)
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"worker_id": "worker-1"}})

	lease, err := provider.AcquireHTTPClient(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state)
	if err != nil {
		t.Fatalf("acquire HTTP client: %v", err)
	}
	defer lease.Release()
	if lease.BaseURL != "https://worker.example" || lease.AuthToken != "worker-token" {
		t.Fatalf("lease = %#v", lease)
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
	client       *http.Client
	baseURL      string
	authToken    string
	workerID     string
	inspectCalls int
}

type existingInstanceDriver struct {
	instanceID   string
	createCalls  int
	inspectCalls int
}

func (d *existingInstanceDriver) CreateVM(context.Context, InstanceSpec) (*Instance, error) {
	d.createCalls++
	return nil, errors.New("CreateVM should not be called for existing state")
}

func (d *existingInstanceDriver) StartVM(context.Context, string) (*Instance, error) {
	return nil, errors.New("StartVM should not be called")
}

func (d *existingInstanceDriver) StopVM(context.Context, string, time.Duration) (*Instance, error) {
	return nil, errors.New("StopVM should not be called")
}

func (d *existingInstanceDriver) DeleteVM(context.Context, string, bool) error {
	return errors.New("DeleteVM should not be called")
}

func (d *existingInstanceDriver) InspectVM(_ context.Context, id string) (*Instance, error) {
	d.inspectCalls++
	if id != d.instanceID {
		return nil, sandbox.ErrNotFound
	}
	return &Instance{ID: id, Status: sandbox.StatusRunning}, nil
}

func (d *workerHTTPOnlyDriver) CreateVM(context.Context, InstanceSpec) (*Instance, error) {
	return nil, errors.New("CreateVM should not be called")
}

func (d *workerHTTPOnlyDriver) StartVM(context.Context, string) (*Instance, error) {
	return nil, errors.New("StartVM should not be called")
}

func (d *workerHTTPOnlyDriver) StopVM(context.Context, string, time.Duration) (*Instance, error) {
	return nil, errors.New("StopVM should not be called")
}

func (d *workerHTTPOnlyDriver) DeleteVM(context.Context, string, bool) error {
	return errors.New("DeleteVM should not be called")
}

func (d *workerHTTPOnlyDriver) InspectVM(context.Context, string) (*Instance, error) {
	d.inspectCalls++
	return nil, errors.New("InspectVM should not be called")
}

func (d *workerHTTPOnlyDriver) AcquireWorkerHTTPClient(_ context.Context, workerID string) (*sandbox.HTTPClientLease, error) {
	d.workerID = workerID
	return sandbox.NewHTTPClientLeaseWithBaseURLAndAuth(d.client, d.baseURL, d.authToken, nil), nil
}

type recordingWorkerStore struct {
	worker      *model.Worker
	workersByID map[string]*model.Worker
	err         error
	sandbox     *model.Sandbox
	findCalls   int
}

func (s *recordingWorkerStore) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return nil, nil
}

func (s *recordingWorkerStore) GetWorker(_ context.Context, workerID string) (*model.Worker, error) {
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

func (s *recordingWorkerStore) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	return worker, nil
}

func (s *recordingWorkerStore) FindSchedulableWorker(_ context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
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
		return nil, apperrors.ErrNotFound
	}
	return s.worker, nil
}

type repairingWorkerStore struct {
	workers        []model.Worker
	jobs           map[string]*orchestration.Job
	updated        *model.Worker
	repairUpdated  bool
	createdWorkers int
}

func (s *repairingWorkerStore) ListWorkers(context.Context, string, string) ([]model.Worker, error) {
	return s.workers, nil
}

func (s *repairingWorkerStore) CreateWorker(_ context.Context, worker *model.Worker) (*model.Worker, error) {
	s.createdWorkers++
	return worker, nil
}

func (s *repairingWorkerStore) FindSchedulableWorker(context.Context, *model.Sandbox) (*model.Worker, error) {
	return nil, apperrors.ErrNotFound
}

func (s *repairingWorkerStore) GetJob(_ context.Context, id string) (*orchestration.Job, error) {
	job := s.jobs[id]
	if job == nil {
		return nil, orchestration.ErrJobNotFound
	}
	return job, nil
}

func (s *repairingWorkerStore) DeleteWorkerForFailedJob(_ context.Context, workerID string, generation int64, jobID string, message string) (bool, error) {
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

func (s *repairingWorkerStore) DeleteWorkerForExpiredRegistration(_ context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
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
