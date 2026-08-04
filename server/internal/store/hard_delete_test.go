package store_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
)

// Deletes are real, not tombstones. Every table below carries a unique index, so
// a soft delete would leave the deleted row occupying it and make recreating the
// same thing fail with a constraint error — deleting a secret would burn its
// (type, host) slot, deleting a pool would burn its name, and so on. These tests
// exist to catch a gorm.DeletedAt field reappearing on any of these models.

func TestDeletedSecretFreesItsUniqueSlot(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	// A user-created secret leaves UniqueKey empty, so its uniqueness domain is
	// (project, type, host) — the case a tombstone would block.
	first := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, Host: "a.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, first); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := s.DeleteSecret(ctx, "project-1", first.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	second := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, Host: "a.example.com", EncryptedValue: []byte(`{"token":"t2"}`)}
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
	s, db := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")

	tokenHash := sha256.Sum256([]byte("pool-bootstrap-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{
		PoolID:    "pool-1",
		TokenHash: tokenHash[:],
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create pool bootstrap token: %v", err)
	}
	if err := s.DeletePool(ctx, "project-1", "pool-1"); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	var tokenCount int64
	if err := db.Write.Model(&model.PoolBootstrapToken{}).Where("pool_id = ?", "pool-1").Count(&tokenCount).Error; err != nil {
		t.Fatalf("count pool bootstrap tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Fatalf("pool bootstrap token count = %d after pool delete, want 0", tokenCount)
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
	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, Host: "a.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	// Deleting the secret cascades to its binding. Rebinding the same env name
	// afterwards is the configure/deconfigure/configure path.
	if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	bindings, err := s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected the binding to be gone, got %d", len(bindings))
	}

	replacement := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, Host: "a.example.com", EncryptedValue: []byte(`{"token":"t2"}`)}
	if err := s.CreateSecret(ctx, replacement); err != nil {
		t.Fatalf("create replacement secret: %v", err)
	}
	if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: replacement.ID,
	}); err != nil {
		t.Fatalf("rebind the same env name after delete: %v", err)
	}
	bindings, err = s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].SecretID != replacement.ID {
		t.Fatalf("expected one binding to the replacement secret, got %+v", bindings)
	}
}
