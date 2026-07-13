package service

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/resources/providers"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestEnqueueProviderWorkersSchedulesEveryWorkerWithDefaultAttempts(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	otherProvider := &model.SandboxProviderInstance{ID: "provider-2", ProjectID: project.ID, Type: "docker", Name: "Other Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, otherProvider); err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	for _, worker := range []model.Worker{
		{ID: "worker-1", ProjectID: project.ID, ProviderInstanceID: provider.ID, Identity: "worker-1"},
		{ID: "worker-2", ProjectID: project.ID, ProviderInstanceID: provider.ID, Identity: "worker-2"},
		{ID: "worker-3", ProjectID: project.ID, ProviderInstanceID: otherProvider.ID, Identity: "worker-3"},
	} {
		worker := worker
		if err := appStore.CreateWorker(ctx, &worker); err != nil {
			t.Fatalf("create worker %s: %v", worker.ID, err)
		}
	}

	engine := newStartedTestReconcileEngine(ctx, t, db)
	svc := New(appStore, engine, JobManagerOptions{})
	if err := svc.enqueueProviderWorkers(ctx, project.ID, provider.ID); err != nil {
		t.Fatalf("enqueue provider workers: %v", err)
	}

	// Worker reconciliation rides the level-triggered reconcile engine: every
	// worker of the target provider must be marked dirty, and no others.
	dirty, err := engine.ListDirty(ctx, workers.WorkerResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	gotWorkers := map[string]bool{}
	for _, mark := range dirty {
		gotWorkers[mark.ResourceID] = true
	}
	for _, id := range []string{"worker-1", "worker-2"} {
		if !gotWorkers[id] {
			t.Fatalf("missing dirty mark for %s; got %#v", id, gotWorkers)
		}
	}
	if gotWorkers["worker-3"] {
		t.Fatalf("marked worker from a different provider: %#v", gotWorkers)
	}
}

func TestEnsureExistingSandboxProviderInstancesSchedulesWorkerProviderReconcile(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	for _, worker := range []model.Worker{
		{ID: "worker-1", ProjectID: project.ID, ProviderInstanceID: provider.ID, Identity: "worker-1"},
		{ID: "worker-2", ProjectID: project.ID, ProviderInstanceID: provider.ID, Identity: "worker-2"},
	} {
		worker := worker
		if err := appStore.CreateWorker(ctx, &worker); err != nil {
			t.Fatalf("create worker %s: %v", worker.ID, err)
		}
	}

	engine := newStartedTestReconcileEngine(ctx, t, db)
	svc := New(appStore, engine, JobManagerOptions{})
	if err := svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		t.Fatalf("ensure existing providers: %v", err)
	}

	// Provider reconciliation now rides the level-triggered reconcile engine:
	// startup must mark the provider dirty rather than append a job row.
	dirty, err := engine.ListDirty(ctx, workers.WorkerProviderResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("dirty provider marks = %d, want 1 (%#v)", len(dirty), dirty)
	}
	if want := workers.WorkerProviderDirtyID(project.ID, provider.ID); dirty[0].ResourceID != want {
		t.Fatalf("dirty provider id = %q, want %q", dirty[0].ResourceID, want)
	}

	workerDirty, err := engine.ListDirty(ctx, workers.WorkerResourceType)
	if err != nil {
		t.Fatalf("list dirty workers: %v", err)
	}
	if len(workerDirty) != 0 {
		t.Fatalf("unexpected worker dirty marks from startup: %#v", workerDirty)
	}
}

func newStartedTestReconcileEngine(ctx context.Context, t *testing.T, db *database.DB) *reconcile.Engine {
	t.Helper()

	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("new reconcile engine: %v", err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start reconcile engine: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})
	return engine
}

func TestListSandboxProviderInstancesIncludesWorkerFailureStatus(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	otherProvider := &model.SandboxProviderInstance{ID: "provider-2", ProjectID: project.ID, Type: "docker", Name: "Other Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, otherProvider); err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	errMessage := "docker create failed"
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          project.ID,
		ProviderInstanceID: provider.ID,
		Identity:           "worker-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseFailed,
			LastOperationStatus: model.OperationStatusFailed,
			ErrorMessage:        &errMessage,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	otherWorker := &model.Worker{
		ID:                 "worker-2",
		ProjectID:          project.ID,
		ProviderInstanceID: otherProvider.ID,
		Identity:           "worker-2",
		Ready:              true,
		Schedulable:        true,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseActive,
			LastOperationStatus: model.OperationStatusSuccess,
		},
	}
	if err := appStore.CreateWorker(ctx, otherWorker); err != nil {
		t.Fatalf("create other worker: %v", err)
	}

	svc := newProviderInstanceTestService(appStore)
	providers, err := svc.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("providers len = %d, want 2", len(providers))
	}
	status := providers[0].Status
	if status == nil {
		t.Fatal("provider status is nil")
	}
	if status.FailedWorkers != 1 || status.WorkerCount != 1 {
		t.Fatalf("provider failed/total workers = %d/%d, want 1/1", status.FailedWorkers, status.WorkerCount)
	}
	if status.LastError == nil || *status.LastError != errMessage {
		t.Fatalf("provider last error = %v, want %q", status.LastError, errMessage)
	}
	if len(status.Workers) != 1 || status.Workers[0].ErrorMessage == nil || *status.Workers[0].ErrorMessage != errMessage {
		t.Fatalf("provider workers = %#v, want failed worker error", status.Workers)
	}
	otherStatus := providers[1].Status
	if otherStatus == nil {
		t.Fatal("other provider status is nil")
	}
	if otherStatus.FailedWorkers != 0 || otherStatus.ReadyWorkers != 1 || otherStatus.WorkerCount != 1 {
		t.Fatalf("other provider failed/ready/total workers = %d/%d/%d, want 0/1/1", otherStatus.FailedWorkers, otherStatus.ReadyWorkers, otherStatus.WorkerCount)
	}
}

func TestListSandboxProviderInstancesIgnoresStaleFailedWorkersWhenCurrentWorkerExists(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	errMessage := "old worker container exited"
	workers := []model.Worker{
		{
			ID:                 "worker-failed",
			ProjectID:          project.ID,
			ProviderInstanceID: provider.ID,
			Identity:           "worker-failed",
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseFailed,
				LastOperationStatus: model.OperationStatusFailed,
				ErrorMessage:        &errMessage,
			},
		},
		{
			ID:                 "worker-ready",
			ProjectID:          project.ID,
			ProviderInstanceID: provider.ID,
			Identity:           "worker-ready",
			Ready:              true,
			Schedulable:        true,
			ResourceLifecycle: model.ResourceLifecycle{
				DesiredState:        model.WorkerDesiredStateActive,
				Phase:               model.WorkerPhaseActive,
				LastOperationStatus: model.OperationStatusSuccess,
			},
		},
	}
	for i := range workers {
		worker := workers[i]
		if err := appStore.CreateWorker(ctx, &worker); err != nil {
			t.Fatalf("create worker %s: %v", worker.ID, err)
		}
	}

	svc := newProviderInstanceTestService(appStore)
	providers, err := svc.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	status := providers[0].Status
	if status == nil {
		t.Fatal("provider status is nil")
	}
	if status.WorkerCount != 1 || status.ReadyWorkers != 1 || status.FailedWorkers != 0 {
		t.Fatalf("provider ready/failed/total workers = %d/%d/%d, want 1/0/1", status.ReadyWorkers, status.FailedWorkers, status.WorkerCount)
	}
	if status.LastError != nil {
		t.Fatalf("provider last error = %q, want nil", *status.LastError)
	}
	if len(status.Workers) != 1 || status.Workers[0].ID != "worker-ready" {
		t.Fatalf("provider workers = %#v, want only current ready worker", status.Workers)
	}
}

func TestListSandboxProviderInstancesTreatsFailedJobCleanupAsFailureStatus(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	message := "worker reconcile job failed"
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          project.ID,
		ProviderInstanceID: provider.ID,
		Identity:           "worker-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateDeleted,
			Phase:               model.WorkerPhaseDeleted,
			ActiveOperation:     stringPtr(model.WorkerDeleteOperation.Operation),
			LastOperationStatus: model.OperationStatusPending,
			StatusMessage:       &message,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	svc := newProviderInstanceTestService(appStore)
	providers, err := svc.ListSandboxProviderInstances(ctx, project.ID)
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	status := providers[0].Status
	if status == nil {
		t.Fatal("provider status is nil")
	}
	if status.FailedWorkers != 0 || status.WorkerCount != 0 {
		t.Fatalf("provider failed/total workers = %d/%d, want 0/0", status.FailedWorkers, status.WorkerCount)
	}
	if status.LastError == nil || *status.LastError != message {
		t.Fatalf("provider last error = %v, want %q", status.LastError, message)
	}
}

func newProviderInstanceTestService(appStore *store.Store) *Service {
	return &Service{
		store:                          appStore,
		SandboxProviderInstanceService: providers.NewService(appStore, nil, nil),
	}
}

func stringPtr(value string) *string {
	return &value
}
