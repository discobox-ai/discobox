package store_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Deletes are real, not tombstones. Every table below carries a unique index, so
// a soft delete would leave the deleted row occupying it and make recreating the
// same thing fail with a constraint error — deleting a secret would burn its
// (type, host) slot, deleting a pool would burn its name, and so on. These tests
// exist to catch a gorm.DeletedAt field reappearing on any of these models.

func TestDeletedSecretFreesItsUniqueSlot(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	// A secret's name is unique in its project — the slot a tombstone would
	// block.
	first := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, first); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := s.DeleteSecret(ctx, "project-1", first.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	second := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t2"}`)}
	if err := s.CreateSecret(ctx, second); err != nil {
		t.Fatalf("recreate secret after delete: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("recreate reused the deleted row instead of making a new one")
	}
	if _, err := s.GetSecret(ctx, "project-1", first.ID); err == nil {
		t.Fatalf("deleted secret is still readable")
	}
}

func TestDeletedPoolFreesItsName(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")

	if err := s.DeletePool(ctx, "project-1", "pool-1"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	// Same project, same name: unique on (project_id, name).
	if err := s.CreatePool(ctx, &model.Pool{ID: "pool-2", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-pool-1"}}); err != nil {
		t.Fatalf("recreate pool with the deleted pool's name: %v", err)
	}
}

func TestDeletedHarnessConfigBindingFreesItsEnvName(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	bind := func(secretID string) error {
		_, err := s.PutTargetUseBinding(ctx, &model.SecretBinding{
			ProjectID: "project-1", SecretID: secretID, TargetKind: model.SecretBindingTargetHarnessConfig,
			TargetID: config.ID, Use: true, EnvName: "OPENAI_API_KEY",
		}, store.BindingChange{Actor: "user-1"})
		return err
	}

	// Deleting the secret takes its binding with it. Rebinding the same env
	// name afterwards is the configure/deconfigure/configure path.
	if err := bind(sec.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	bindings, err := s.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected the binding to be gone, got %d", len(bindings))
	}
	events, err := s.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{SecretID: sec.ID})
	if err != nil || len(events) != 2 || events[0].Event != model.SecretBindingEventDeleted || events[0].Actor != model.SecretBindingActorSystem {
		t.Fatalf("events = %+v, %v; want the removal recorded as the system's after the creation", events, err)
	}

	replacement := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t2"}`)}
	if err := s.CreateSecret(ctx, replacement); err != nil {
		t.Fatalf("create replacement secret: %v", err)
	}
	if err := bind(replacement.ID); err != nil {
		t.Fatalf("rebind the same env name after delete: %v", err)
	}
	bindings, err = s.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].SecretID != replacement.ID {
		t.Fatalf("expected one binding to the replacement secret, got %+v", bindings)
	}
}
