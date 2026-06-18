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
