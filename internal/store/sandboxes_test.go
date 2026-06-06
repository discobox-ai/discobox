package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/store"
)

func TestGetSandboxWithGeneration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sandbox := &model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		Name:            "alpha",
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	got, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(sandbox.Generation))
	if err != nil {
		t.Fatalf("get matching generation: %v", err)
	}
	if got.ID != sandbox.ID {
		t.Fatalf("sandbox id = %q, want %q", got.ID, sandbox.ID)
	}

	if _, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID, store.WithGeneration(sandbox.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("get stale generation error = %v, want ErrGenerationConflict", err)
	}

	sandbox.Name = "renamed"
	if err := s.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation)); err != nil {
		t.Fatalf("update matching generation: %v", err)
	}

	sandbox.Name = "stale"
	if err := s.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("update stale generation error = %v, want ErrGenerationConflict", err)
	}
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()

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

	user := &model.User{
		ID:       "user-1",
		Email:    "user@example.com",
		Provider: "test",
		Subject:  "user",
	}
	if err := db.Write.WithContext(ctx).Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	project := &model.Project{
		ID:          "project-1",
		OwnerUserID: user.ID,
		Name:        "Project",
		Slug:        "project",
	}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return store.New(db.Write, db.Read)
}
