package workerpool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workeragent "github.com/obot-platform/discobox/worker-agent"
	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
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

// TestDesiredAdditionalWorkersKeepsFailedWorkerInItsSlot pins the bound that
// stops the pool from spawning workers without limit. A failed worker occupies
// its slot and is retried in place, so a full pool launches no replacement.
// Counting it as absent instead made the pool replace it, the replacement fail
// identically, and the dead rows — invisible to both Min and Max — accumulate
// forever (hundreds a minute against an unpullable image).
func TestDesiredAdditionalWorkersKeepsFailedWorkerInItsSlot(t *testing.T) {
	workers := []model.Worker{{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}

	if got := desiredAdditionalWorkers(workers, cfg); got != 0 {
		t.Fatalf("additional workers = %d, want 0 for a full pool whose worker is retried in place", got)
	}
}

// TestDesiredAdditionalWorkersReplacesFailedWorkerWithinMax keeps replacement
// working where there is room for it: Max now genuinely bounds the launches.
func TestDesiredAdditionalWorkersReplacesFailedWorkerWithinMax(t *testing.T) {
	workers := []model.Worker{{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}}
	cfg := WorkerPoolConfig{Min: 1, Max: 2, MinHealthy: 1}

	if got := desiredAdditionalWorkers(workers, cfg); got != 1 {
		t.Fatalf("additional workers = %d, want 1 healthy replacement within Max", got)
	}
}

// TestEnsureWorkerPoolLeavesInFlightOperationsAlone pins the level-triggered
// contract: a worker whose recorded operation is still in flight belongs to
// the reconcile engine (dirty row + lease recovery), so the pool must not
// touch it — no failure latching, no re-marking, no replacement.
func TestEnsureWorkerPoolLeavesInFlightOperationsAlone(t *testing.T) {
	store := &repairingWorkerManager{
		workers: []model.Worker{{
			ID:                 "worker-1",
			ProjectID:          "project-1",
			ProviderInstanceID: "provider-1",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseLaunching,
				LastOperationStatus: model.OperationStatusRunning,
			},
		}},
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if store.updated != nil {
		t.Fatalf("updated worker = %#v, want untouched in-flight operation", store.updated)
	}
	if len(store.scheduledReconciliations) != 0 || len(store.scheduledRepairs) != 0 {
		t.Fatalf("scheduled reconciliations = %v, repairs = %v, want none for in-flight operation", store.scheduledReconciliations, store.scheduledRepairs)
	}
	if store.createdWorkers != 0 {
		t.Fatalf("created workers = %d, want none while operation is in flight", store.createdWorkers)
	}
}

func TestEnsureWorkerPoolReDrivesFailedCreatedWorker(t *testing.T) {
	registeredAt := time.Now().UTC()
	store := &repairingWorkerManager{
		workers: []model.Worker{{
			ID:                 "worker-1",
			ProjectID:          "project-1",
			ProviderInstanceID: "provider-1",
			RegisteredAt:       &registeredAt,
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseOffline,
				LastOperationStatus: model.OperationStatusFailed,
			},
		}},
		repairUpdated: true,
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	// A repair is new intent, not a plain dirty mark: it must bump the worker's
	// generation so schedulers can tell a pending retry from a settled failure.
	if len(store.scheduledRepairs) != 1 || store.scheduledRepairs[0] != "worker-1" {
		t.Fatalf("scheduled repairs = %v, want [worker-1]", store.scheduledRepairs)
	}
	if store.updated != nil {
		t.Fatalf("updated worker = %#v, want no terminal failure for created worker", store.updated)
	}
}

// TestActiveWorkerKeepsFailedWorkersInThePool pins that a failed worker holds
// its slot whether or not its runtime was ever created, so the pool retries it
// rather than replacing it without bound. Neither counts as healthy capacity,
// so nothing is scheduled onto it while it recovers.
func TestActiveWorkerKeepsFailedWorkersInThePool(t *testing.T) {
	registeredAt := time.Now().UTC()
	created := &model.Worker{
		RegisteredAt: &registeredAt,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseOffline,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}
	neverCreated := &model.Worker{
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
		},
	}

	for name, worker := range map[string]*model.Worker{"created": created, "never created": neverCreated} {
		t.Run(name, func(t *testing.T) {
			if !activeWorker(worker) {
				t.Fatal("failed worker should keep its pool slot and be retried in place")
			}
			if healthyWorker(worker) {
				t.Fatal("failed worker should not count as healthy capacity")
			}
		})
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

func TestEnsureWorkerPoolDeletesExcessActiveWorkers(t *testing.T) {
	oldSeen := time.Now().UTC().Add(-time.Minute)
	newSeen := time.Now().UTC()
	store := &repairingWorkerManager{
		workers: []model.Worker{
			{
				ID:                 "worker-old",
				ProjectID:          "project-1",
				ProviderInstanceID: "provider-1",
				Ready:              true,
				Schedulable:        true,
				LastSeenAt:         &oldSeen,
				ResourceLifecycle: model.ResourceLifecycle{
					DesiredState:        model.WorkerDesiredStateActive,
					Phase:               model.WorkerPhaseActive,
					LastOperationStatus: model.OperationStatusSuccess,
				},
			},
			{
				ID:                 "worker-new",
				ProjectID:          "project-1",
				ProviderInstanceID: "provider-1",
				Ready:              true,
				Schedulable:        true,
				LastSeenAt:         &newSeen,
				ResourceLifecycle: model.ResourceLifecycle{
					DesiredState:        model.WorkerDesiredStateActive,
					Phase:               model.WorkerPhaseActive,
					LastOperationStatus: model.OperationStatusSuccess,
				},
			},
			{
				ID:                 "worker-launching",
				ProjectID:          "project-1",
				ProviderInstanceID: "provider-1",
				ResourceLifecycle: model.ResourceLifecycle{
					DesiredState:        model.WorkerDesiredStateActive,
					Phase:               model.WorkerPhaseLaunching,
					LastOperationStatus: model.OperationStatusRunning,
				},
			},
		},
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if !reflect.DeepEqual(store.deletedWorkerIDs, []string{"worker-launching", "worker-old"}) {
		t.Fatalf("deleted workers = %#v, want launching then old", store.deletedWorkerIDs)
	}
	if store.createdWorkers != 0 {
		t.Fatalf("created workers = %d, want none", store.createdWorkers)
	}
}

func TestEnsureWorkerPoolSkipsExcessWorkersWithAssignedSandboxes(t *testing.T) {
	store := &repairingWorkerManager{
		assignedSandboxes: map[string]int64{"worker-occupied": 1},
		workers: []model.Worker{
			{
				ID:                 "worker-empty",
				ProjectID:          "project-1",
				ProviderInstanceID: "provider-1",
				Ready:              true,
				Schedulable:        true,
				ResourceLifecycle: model.ResourceLifecycle{
					DesiredState:        model.WorkerDesiredStateActive,
					Phase:               model.WorkerPhaseActive,
					LastOperationStatus: model.OperationStatusSuccess,
				},
			},
			{
				ID:                 "worker-occupied",
				ProjectID:          "project-1",
				ProviderInstanceID: "provider-1",
				ResourceLifecycle: model.ResourceLifecycle{
					DesiredState:        model.WorkerDesiredStateActive,
					Phase:               model.WorkerPhaseLaunching,
					LastOperationStatus: model.OperationStatusRunning,
				},
			},
		},
	}
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := ensureWorkerPool(context.Background(), store, project, provider, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}); err != nil {
		t.Fatalf("ensure worker pool: %v", err)
	}

	if !reflect.DeepEqual(store.deletedWorkerIDs, []string{"worker-empty"}) {
		t.Fatalf("deleted workers = %#v, want only empty worker", store.deletedWorkerIDs)
	}
}

// TestWorkerPoolProviderReconcileWorkerSkipsTimeoutForRegisteredWorker pins the
// bound on the busy loop. The provider reconcile drift-checks every healthy
// worker through ReconcileWorker, so arming the registration timeout there
// re-marks the provider row being reconciled: MarkDirtyAt keeps the row
// immediately runnable, bumps its seq past the settle, and wakes the claim loop,
// which re-runs the reconcile at once — hundreds of times a second. Only a
// worker that has never registered can time out, so only it arms the timer.
func TestWorkerPoolProviderReconcileWorkerSkipsTimeoutForRegisteredWorker(t *testing.T) {
	oldTimeout := workerRegistrationTimeout
	workerRegistrationTimeout = time.Minute
	t.Cleanup(func() { workerRegistrationTimeout = oldTimeout })

	registeredAt := time.Now().UTC()
	workerManager := &recordingWorkerManager{}
	pool := New(&testWorkerProvider{}, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", RegisteredAt: &registeredAt}

	if err := pool.ReconcileWorker(context.Background(), workerManager, project, provider, worker); err != nil {
		t.Fatalf("reconcile worker: %v", err)
	}

	if !workerManager.scheduledProviderAt.IsZero() || workerManager.scheduledProviderID != "" {
		t.Fatalf("scheduled provider reconcile at %s for %q, want none for an already-registered worker",
			workerManager.scheduledProviderAt, workerManager.scheduledProviderID)
	}
}

// TestWorkerPoolProviderMintsBootstrapOnlyWhenRuntimeIsCreated pins the fix for
// an unbounded token leak. Minting persists a single-use bootstrap token, and
// the provider reconcile drift-checks every healthy worker through
// ReconcileWorker, so minting eagerly wrote one token row per reconcile — 9.7
// million of them, and 3.4GB of database, on a pool of one worker.
func TestWorkerPoolProviderMintsBootstrapOnlyWhenRuntimeIsCreated(t *testing.T) {
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1"}

	t.Run("drift check over a healthy runtime", func(t *testing.T) {
		workerManager := &recordingWorkerManager{}
		pool := New(&testWorkerProvider{}, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

		if err := pool.ReconcileWorker(context.Background(), workerManager, project, provider, worker); err != nil {
			t.Fatalf("reconcile worker: %v", err)
		}

		if workerManager.mintedBootstrapTokens != 0 {
			t.Fatalf("minted %d bootstrap tokens, want none when no runtime is created", workerManager.mintedBootstrapTokens)
		}
	})

	t.Run("runtime creation", func(t *testing.T) {
		workerManager := &recordingWorkerManager{}
		pool := New(&testWorkerProvider{createsRuntime: true}, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

		if err := pool.ReconcileWorker(context.Background(), workerManager, project, provider, worker); err != nil {
			t.Fatalf("reconcile worker: %v", err)
		}

		if workerManager.mintedBootstrapTokens != 1 {
			t.Fatalf("minted %d bootstrap tokens, want exactly 1 for a created runtime", workerManager.mintedBootstrapTokens)
		}
	})
}

func TestNormalizeWorkerPoolConfigKeepsPoolSizeAsMinimumWithReplacementHeadroom(t *testing.T) {
	cfg := NormalizeWorkerPoolConfig(1, 0, 0, 0)
	if cfg.Min != 1 || cfg.Max != 2 || cfg.MinHealthy != 1 {
		t.Fatalf("config = %#v, want min=1 max=2 minHealthy=1", cfg)
	}
}

func TestWorkerPoolProviderReconcileRunsCapacity(t *testing.T) {
	workerManager := &recordingWorkerManager{}
	workerProvider := &testWorkerProvider{}
	pool := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{Max: 1}, workerManager)
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}

	if err := pool.ReconcileWorkerProvider(context.Background(), workerManager, project, provider); err != nil {
		t.Fatalf("reconcile worker provider: %v", err)
	}

	// Provider reconcile sizes the pool and then re-ensures active workers.
	if workerManager.listCalls != 2 {
		t.Fatalf("list calls after reconcile = %d, want 2", workerManager.listCalls)
	}
}

func TestWorkerPoolProviderReconcileWorkerSchedulesRegistrationTimeout(t *testing.T) {
	oldTimeout := workerRegistrationTimeout
	workerRegistrationTimeout = time.Minute
	t.Cleanup(func() { workerRegistrationTimeout = oldTimeout })

	workerManager := &recordingWorkerManager{}
	workerProvider := &testWorkerProvider{}
	pool := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)
	project := &model.Project{ID: "project-1"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1"}

	before := time.Now().UTC().Add(workerRegistrationTimeout)
	if err := pool.ReconcileWorker(context.Background(), workerManager, project, provider, worker); err != nil {
		t.Fatalf("reconcile worker: %v", err)
	}
	after := time.Now().UTC().Add(workerRegistrationTimeout)

	if workerManager.scheduledProviderProjectID != "project-1" || workerManager.scheduledProviderID != "provider-1" {
		t.Fatalf("scheduled provider reconcile = %q/%q, want project-1/provider-1", workerManager.scheduledProviderProjectID, workerManager.scheduledProviderID)
	}
	if workerManager.scheduledProviderAt.Before(before) || workerManager.scheduledProviderAt.After(after) {
		t.Fatalf("scheduled provider reconcile at = %s, want between %s and %s", workerManager.scheduledProviderAt, before, after)
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
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

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
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: false, Degraded: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	provider := New(driver, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

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

func TestMapWorkerClientErrorUsesProblemDetail(t *testing.T) {
	err := mapWorkerClientError(&workerclient.ErrorModelStatusCode{
		StatusCode: http.StatusInternalServerError,
		Response: workerclient.ErrorModel{
			Status: workerclient.NewOptInt64(http.StatusInternalServerError),
			Title:  workerclient.NewOptString(http.StatusText(http.StatusInternalServerError)),
			Detail: workerclient.NewOptString("materialize source \"repo\": git clone failed"),
		},
	})

	if got, want := err.Error(), `worker-agent request failed: materialize source "repo": git clone failed`; got != want {
		t.Fatalf("mapped error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "Schema:") || strings.Contains(err.Error(), "Detail:{") {
		t.Fatalf("mapped error still contains generated struct formatting: %q", err.Error())
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
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "worker-runtime", Metadata: map[string]string{"worker_id": "worker-1"}})
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
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "worker-runtime", Metadata: map[string]string{"worker_id": "worker-1"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-1": {ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

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
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "worker-runtime", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-2", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-2")
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{ProviderInstanceID: "provider-1", WorkerID: "worker-2"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
	if workerManager.findCalls != 0 {
		t.Fatalf("FindSchedulableWorker calls = %d, want 0", workerManager.findCalls)
	}
}

func TestWorkerProviderCreateWithUnassignedStateSchedulesWorker(t *testing.T) {
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "worker-runtime", Metadata: map[string]string{"worker_id": "worker-2"}})
	workerManager := &recordingWorkerManager{
		worker: &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true},
		workersByID: map[string]*model.Worker{
			"worker-2": {ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: "provider-1", Ready: true, Schedulable: true, ResourceLifecycle: model.ResourceLifecycle{Phase: model.WorkerPhaseActive, LastOperationStatus: model.OperationStatusSuccess}},
		},
	}
	workerProvider := newTestWorkerProvider(t, "project-1", "worker-1")
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)

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
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, workerManager)

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
	provider := New(workerProvider, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{Min: 1, Max: 1, MinHealthy: 1}, workerManager)

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
		if err := json.NewEncoder(w).Encode(&workerclient.WorkerSandboxInstance{
			SandboxId: "sandbox-1",
			Config:    workerclient.SandboxConfig{},
			Runtime: workerclient.WorkerSandboxRuntime{
				InstanceId: "runtime-1",
				Status:     string(sandbox.StatusRunning),
				Image:      "image-1",
				CreatedAt:  time.Now().UTC(),
				StartedAt:  workerclient.NilDateTime{Null: true},
				StoppedAt:  workerclient.NilDateTime{Null: true},
				Metadata:   map[string]string{},
				Env:        map[string]string{},
				Ports:      []workerclient.WorkerSandboxPort{},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	driver := &workerHTTPOnlyDriver{baseURL: server.URL, client: server.Client(), authToken: "worker-token"}
	workerManager := &recordingWorkerManager{worker: &model.Worker{ID: "worker-1"}}
	provider := New(driver, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"worker_id": "worker-1"}})

	runtimeSandbox, err := provider.Get(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if runtimeSandbox.ID != "runtime-1" || runtimeSandbox.Metadata["worker_id"] != "worker-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
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
	workerManager := &recordingWorkerManager{worker: &model.Worker{ID: "worker-1"}}
	provider := New(driver, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)
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
}

func TestWorkerProviderAcquireHTTPClientReconcilesWorkerAndRetries(t *testing.T) {
	oldTimeout := workerCapacityWaitTimeout
	oldInterval := workerCapacityPollInterval
	workerCapacityWaitTimeout = 50 * time.Millisecond
	workerCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		workerCapacityWaitTimeout = oldTimeout
		workerCapacityPollInterval = oldInterval
	})

	driver := &workerHTTPOnlyDriver{
		client:      http.DefaultClient,
		baseURL:     "https://worker.example",
		authToken:   "worker-token",
		acquireErrs: []error{sandbox.ErrNotFound},
	}
	jobID := "worker-job-1"
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseActive,
			LastOperationStatus: model.OperationStatusSuccess,
		},
	}
	workerManager := &recordingWorkerManager{
		worker:               worker,
		scheduledWorkerJobID: jobID,
	}
	provider := New(driver, sandbox.ProviderDefinition{Name: "test"}, WorkerPoolConfig{}, workerManager)
	state := workerRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"worker_id": "worker-1"}})

	lease, err := provider.AcquireHTTPClient(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, []string{workeragentauth.ScopeSandboxRead})
	if err != nil {
		t.Fatalf("acquire HTTP client: %v", err)
	}
	defer lease.Release()
	if workerManager.scheduleWorkerCalls != 1 {
		t.Fatalf("scheduled worker reconciles = %d, want 1", workerManager.scheduleWorkerCalls)
	}
	if driver.acquireCalls != 2 {
		t.Fatalf("AcquireWorkerHTTPClient calls = %d, want 2", driver.acquireCalls)
	}
	if lease.BaseURL != "https://worker.example" {
		t.Fatalf("lease base URL = %q, want worker URL", lease.BaseURL)
	}
}

func ptrString(value string) *string { return &value }

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
	acquireCalls      int
	acquireErrs       []error
}

func (d *workerHTTPOnlyDriver) Close() error {
	return nil
}

func (d *workerHTTPOnlyDriver) EnsureWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker, workeragent.MintBootstrap) error {
	return errors.New("EnsureWorker should not be called")
}

func (d *workerHTTPOnlyDriver) RepairWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker, workeragent.MintBootstrap, string) error {
	return errors.New("RepairWorker should not be called")
}

func (d *workerHTTPOnlyDriver) RemoveWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return errors.New("RemoveWorker should not be called")
}

func (d *workerHTTPOnlyDriver) AcquireWorkerAgentClient(_ context.Context, worker *model.Worker) (*transport.HTTPClientLease, error) {
	workerID := worker.ID
	d.acquireCalls++
	d.workerID = workerID
	if len(d.acquireErrs) > 0 {
		err := d.acquireErrs[0]
		d.acquireErrs = d.acquireErrs[1:]
		if err != nil {
			return nil, err
		}
	}
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
	// createsRuntime makes EnsureWorker mint, standing in for a provider that
	// actually creates a container. Left false, it stands in for a drift check
	// that finds a healthy runtime and needs no credentials.
	createsRuntime bool
}

func (p *testWorkerProvider) Close() error {
	return nil
}

func (p *testWorkerProvider) EnsureWorker(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Worker, mint workeragent.MintBootstrap) error {
	if !p.createsRuntime {
		return nil
	}
	_, err := mint(ctx)
	return err
}

func (p *testWorkerProvider) RepairWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker, workeragent.MintBootstrap, string) error {
	return nil
}

func (p *testWorkerProvider) RemoveWorker(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return nil
}

func (p *testWorkerProvider) AcquireWorkerAgentClient(context.Context, *model.Worker) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(p.client, p.baseURL, func(context.Context) (string, error) {
		return p.token, nil
	}, nil), nil
}

type recordingWorkerManager struct {
	worker                     *model.Worker
	workersByID                map[string]*model.Worker
	err                        error
	sandbox                    *model.Sandbox
	findCalls                  int
	listCalls                  int
	scheduleWorkerCalls        int
	scheduleRepairCalls        int
	mintedBootstrapTokens      int
	workerAgentTokenClaims     []workeragentauth.TokenClaims
	sandboxAgentTokenClaims    []workeragentauth.TokenClaims
	scheduledWorkerJobID       string
	scheduledProviderProjectID string
	scheduledProviderID        string
	scheduledProviderAt        time.Time
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

func (s *recordingWorkerManager) DeleteWorker(context.Context, string) (*model.Worker, error) {
	return nil, nil
}

func (s *recordingWorkerManager) CreateWorkerBootstrapToken(context.Context, *model.WorkerBootstrapToken) error {
	s.mintedBootstrapTokens++
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

func (s *recordingWorkerManager) ScheduleWorkerProviderReconciliationAt(_ context.Context, projectID, providerID string, scheduledAt time.Time) error {
	s.scheduledProviderProjectID = projectID
	s.scheduledProviderID = providerID
	s.scheduledProviderAt = scheduledAt
	return nil
}

func (s *recordingWorkerManager) ScheduleWorkerReconciliation(context.Context, string) error {
	s.scheduleWorkerCalls++
	return nil
}

func (s *recordingWorkerManager) ScheduleWorkerRepair(context.Context, string, string) error {
	s.scheduleRepairCalls++
	return nil
}

func (s *recordingWorkerManager) CountSandboxesForWorkers(_ context.Context, workerIDs []string) (map[string]int64, error) {
	return make(map[string]int64, len(workerIDs)), nil
}

func (s *recordingWorkerManager) CountSandboxesForWorker(context.Context, string) (int64, error) {
	return 0, nil
}

func (s *recordingWorkerManager) GetProject(context.Context, string) (*model.Project, error) {
	return &model.Project{ID: "project-1"}, nil
}

func (s *recordingWorkerManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, nil
}

func (s *recordingWorkerManager) DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error) {
	return false, nil
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

func (s *capacityWaitWorkerManager) DeleteWorker(context.Context, string) (*model.Worker, error) {
	return nil, nil
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

func (s *capacityWaitWorkerManager) ScheduleWorkerProviderReconciliationAt(context.Context, string, string, time.Time) error {
	return nil
}

func (s *capacityWaitWorkerManager) ScheduleWorkerReconciliation(context.Context, string) error {
	return nil
}

func (s *capacityWaitWorkerManager) ScheduleWorkerRepair(context.Context, string, string) error {
	return nil
}

func (s *capacityWaitWorkerManager) CountSandboxesForWorkers(_ context.Context, workerIDs []string) (map[string]int64, error) {
	return make(map[string]int64, len(workerIDs)), nil
}

func (s *capacityWaitWorkerManager) CountSandboxesForWorker(context.Context, string) (int64, error) {
	return 0, nil
}

func (s *capacityWaitWorkerManager) DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error) {
	return false, nil
}

type repairingWorkerManager struct {
	workers                  []model.Worker
	updated                  *model.Worker
	repairUpdated            bool
	createdWorkers           int
	deletedWorkerIDs         []string
	assignedSandboxes        map[string]int64
	scheduledReconciliations []string
	scheduledRepairs         []string
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

func (s *repairingWorkerManager) DeleteWorker(_ context.Context, workerID string) (*model.Worker, error) {
	s.deletedWorkerIDs = append(s.deletedWorkerIDs, workerID)
	for i := range s.workers {
		if s.workers[i].ID != workerID {
			continue
		}
		s.workers[i].IncrementGeneration()
		s.workers[i].BeginOperation(model.WorkerDeleteOperation)
		return &s.workers[i], nil
	}
	return nil, apperrors.ErrNotFound
}

func (s *repairingWorkerManager) CountSandboxesForWorker(_ context.Context, workerID string) (int64, error) {
	if s.assignedSandboxes == nil {
		return 0, nil
	}
	return s.assignedSandboxes[workerID], nil
}

func (s *repairingWorkerManager) CountSandboxesForWorkers(_ context.Context, workerIDs []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(workerIDs))
	for _, workerID := range workerIDs {
		if s.assignedSandboxes != nil {
			counts[workerID] = s.assignedSandboxes[workerID]
		}
	}
	return counts, nil
}

func (s *repairingWorkerManager) GetProject(context.Context, string) (*model.Project, error) {
	return &model.Project{ID: "project-1"}, nil
}

func (s *repairingWorkerManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, nil
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

func (s *repairingWorkerManager) ScheduleWorkerProviderReconciliationAt(context.Context, string, string, time.Time) error {
	return nil
}

func (s *repairingWorkerManager) ScheduleWorkerReconciliation(_ context.Context, workerID string) error {
	s.scheduledReconciliations = append(s.scheduledReconciliations, workerID)
	return nil
}

func (s *repairingWorkerManager) ScheduleWorkerRepair(_ context.Context, workerID, _ string) error {
	s.scheduledRepairs = append(s.scheduledRepairs, workerID)
	for i := range s.workers {
		if s.workers[i].ID == workerID {
			s.workers[i].IncrementGeneration()
		}
	}
	return nil
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
	copied.BeginOperation(model.WorkerDeleteOperation)
	copied.StatusMessage = &message
	s.updated = &copied
	return true, nil
}
