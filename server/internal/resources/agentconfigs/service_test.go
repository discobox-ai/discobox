package agentconfigs_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/agentconfigs"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newBindingService(t *testing.T) (*agentconfigs.Service, *store.Store, string) {
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
	config := &model.AgentConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := st.CreateAgentConfig(ctx, config); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	return agentconfigs.NewService(st), st, config.ID
}

func badRequest(t *testing.T, err error) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestSetAgentConfigSecretBindingValidates(t *testing.T) {
	ctx := context.Background()
	svc, st, configID := newBindingService(t)
	sec := &model.Secret{ProjectID: "project-1", Name: "openai", Type: model.SecretTypeBearer, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	if _, err := svc.SetAgentConfigSecretBinding(ctx, "project-1", configID, "1BAD", sec.ID); err == nil {
		t.Fatal("expected invalid env name to fail")
	} else {
		badRequest(t, err)
	}

	if _, err := svc.SetAgentConfigSecretBinding(ctx, "project-1", configID, "OPENAI_API_KEY", "does-not-exist"); err == nil {
		t.Fatal("expected missing secret to fail")
	}

	binding, err := svc.SetAgentConfigSecretBinding(ctx, "project-1", configID, "OPENAI_API_KEY", sec.ID)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if binding.EnvName != "OPENAI_API_KEY" || binding.SecretID != sec.ID {
		t.Fatalf("binding = %#v", binding)
	}
}
