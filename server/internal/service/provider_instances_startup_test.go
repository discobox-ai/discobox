package service

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/sandbox/jobs"
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

	notifyCount := 0
	workerSubmitter := jobs.NewWorkerSubmitter(appStore, orchestration.QueueConfig{DefaultMaxAttempts: 5}, func(context.Context) { notifyCount++ })
	svc := &Service{store: appStore, workerSubmitter: workerSubmitter}
	if err := svc.enqueueProviderWorkers(ctx, project.ID, provider.ID); err != nil {
		t.Fatalf("enqueue provider workers: %v", err)
	}

	queued, err := appStore.ListJobsForProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	gotWorkers := map[string]bool{}
	for _, job := range queued {
		if job.Type != jobs.WorkerReconcileType {
			continue
		}
		if job.MaxAttempts != 5 {
			t.Fatalf("worker job %s max attempts = %d, want 5", job.ID, job.MaxAttempts)
		}
		gotWorkers[job.Resource.ID] = true
	}
	for _, id := range []string{"worker-1", "worker-2"} {
		if !gotWorkers[id] {
			t.Fatalf("missing reconcile job for %s; got %#v", id, gotWorkers)
		}
	}
	if gotWorkers["worker-3"] {
		t.Fatalf("queued worker from a different provider: %#v", gotWorkers)
	}
	if notifyCount != 2 {
		t.Fatalf("notify count = %d, want 2", notifyCount)
	}
	for _, id := range []string{"worker-1", "worker-2"} {
		worker, err := appStore.GetWorker(ctx, id)
		if err != nil {
			t.Fatalf("get worker %s: %v", id, err)
		}
		if worker.LastJobID == nil || *worker.LastJobID == "" {
			t.Fatalf("worker %s last job ID was not set", id)
		}
	}
}

func TestEnsureExistingSandboxProviderInstancesSchedulesProviderReconcile(t *testing.T) {
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

	notifyCount := 0
	providerSubmitter := jobs.NewProviderSubmitter(appStore, orchestration.QueueConfig{DefaultMaxAttempts: 5}, func(context.Context) { notifyCount++ })
	svc := &Service{store: appStore, providerSubmitter: providerSubmitter}
	if err := svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		t.Fatalf("ensure existing providers: %v", err)
	}

	queued, err := appStore.ListJobsForProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var providerJobs int
	for _, job := range queued {
		switch job.Type {
		case jobs.ProviderReconcileType:
			providerJobs++
			if job.Resource.Type != "provider" || job.Resource.ID != provider.ID {
				t.Fatalf("provider job resource = %#v, want provider %s", job.Resource, provider.ID)
			}
			if job.MaxAttempts != 5 {
				t.Fatalf("provider job max attempts = %d, want 5", job.MaxAttempts)
			}
		case jobs.WorkerReconcileType:
			t.Fatalf("unexpected worker reconcile job %s from startup", job.ID)
		}
	}
	if providerJobs != 1 {
		t.Fatalf("provider jobs = %d, want 1", providerJobs)
	}
	if notifyCount != 1 {
		t.Fatalf("notify count = %d, want 1", notifyCount)
	}
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

	svc := &Service{store: appStore}
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

	svc := &Service{store: appStore}
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

func stringPtr(value string) *string {
	return &value
}
