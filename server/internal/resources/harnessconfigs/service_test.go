package harnessconfigs_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/harnessconfigs"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newBindingService(t *testing.T) (*harnessconfigs.Service, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	return harnessconfigs.NewService(st), st, config.ID
}

func badRequest(t *testing.T, err error) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestSetHarnessConfigSecretBindingValidates(t *testing.T) {
	ctx := context.Background()
	svc, st, configID := newBindingService(t)
	sec := &model.Secret{ProjectID: "project-1", Name: "openai", Type: model.SecretTypeBearer, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := svc.SetHarnessConfigSecretBinding(ctx, "project-1", configID, "1BAD", sec.ID); err == nil {
		t.Fatal("expected invalid env name to fail")
	} else {
		badRequest(t, err)
	}

	if _, err := svc.SetHarnessConfigSecretBinding(ctx, "project-1", configID, "OPENAI_API_KEY", "does-not-exist"); err == nil {
		t.Fatal("expected missing secret to fail")
	}

	binding, err := svc.SetHarnessConfigSecretBinding(ctx, "project-1", configID, "OPENAI_API_KEY", sec.ID)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if binding.EnvName != "OPENAI_API_KEY" || binding.SecretID != sec.ID {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestDeleteDefaultHarnessConfigClearsProjectDefault(t *testing.T) {
	ctx := context.Background()
	svc, st, configID := newBindingService(t)

	project, err := st.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	project.DefaultHarnessConfigID = configID
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set default harness config: %v", err)
	}

	if err := svc.DeleteHarnessConfig(ctx, project.ID, configID); err != nil {
		t.Fatalf("delete default harness config: %v", err)
	}

	project, err = st.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("get project after delete: %v", err)
	}
	if project.DefaultHarnessConfigID != "" {
		t.Fatalf("default harness config ID = %q, want empty", project.DefaultHarnessConfigID)
	}
	if _, err := st.GetHarnessConfig(ctx, project.ID, configID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted harness config = %v, want not found", err)
	}
}
