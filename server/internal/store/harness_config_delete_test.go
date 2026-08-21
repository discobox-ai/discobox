package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestDeleteHarnessConfigSandboxReferences(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")

	newConfig := func(slug string) *model.HarnessConfig {
		cfg := &model.HarnessConfig{ProjectID: "project-1", Slug: slug, Name: slug, RunCommand: []string{"x"}}
		if err := s.CreateHarnessConfig(ctx, cfg); err != nil {
			t.Fatalf("create harness config: %v", err)
		}
		return cfg
	}
	newSandbox := func(id, harnessConfigID string) {
		if err := s.CreateSandbox(ctx, &model.Sandbox{
			ID:              id,
			ProjectID:       "project-1",
			PoolID:          "pool-1",
			CreatedByUserID: "user-1",
			Name:            id,
			SandboxManifest: model.SandboxManifest{HarnessConfigID: &harnessConfigID},
		}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}

	// A live sandbox referencing the config blocks deletion with ErrInUse.
	live := newConfig("live")
	newSandbox("sb-live", live.ID)
	if err := s.DeleteHarnessConfig(ctx, "project-1", live.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete with live sandbox = %v, want ErrInUse", err)
	}

	// A soft-deleted sandbox must NOT block deletion (the FK is cleared).
	stale := newConfig("stale")
	newSandbox("sb-stale", stale.ID)
	if err := db.Write.WithContext(ctx).Delete(&model.Sandbox{}, "id = ?", "sb-stale").Error; err != nil {
		t.Fatalf("soft-delete sandbox: %v", err)
	}
	if err := s.DeleteHarnessConfig(ctx, "project-1", stale.ID); err != nil {
		t.Fatalf("delete with only a soft-deleted sandbox = %v, want success", err)
	}
	if _, err := s.GetHarnessConfig(ctx, "project-1", stale.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config should be gone, got %v", err)
	}
}
