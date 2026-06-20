package service

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestListWorkersFiltersByProvider(t *testing.T) {
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
	providers := []model.SandboxProviderInstance{
		{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"},
		{ID: "provider-2", ProjectID: project.ID, Type: "docker", Name: "Other Docker"},
	}
	for i := range providers {
		if err := appStore.CreateSandboxProviderInstance(ctx, &providers[i]); err != nil {
			t.Fatalf("create provider %s: %v", providers[i].ID, err)
		}
	}
	workers := []model.Worker{
		{ID: "worker-1", ProjectID: project.ID, ProviderInstanceID: providers[0].ID, Identity: "worker-1"},
		{ID: "worker-2", ProjectID: project.ID, ProviderInstanceID: providers[1].ID, Identity: "worker-2"},
	}
	for i := range workers {
		if err := appStore.CreateWorker(ctx, &workers[i]); err != nil {
			t.Fatalf("create worker %s: %v", workers[i].ID, err)
		}
	}

	svc := New(appStore, orchestration.QueueConfig{}, nil)
	filtered, err := svc.ListWorkers(ctx, project.ID, providers[0].ID)
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != workers[0].ID {
		t.Fatalf("filtered workers = %#v, want %s only", filtered, workers[0].ID)
	}
	all, err := svc.ListWorkers(ctx, project.ID, "")
	if err != nil {
		t.Fatalf("list all workers: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all workers len = %d, want 2", len(all))
	}
}
