package store_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestHarnessConfigSecretBindingUpsertListDelete(t *testing.T) {
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
	other := &model.Secret{ProjectID: "project-1", Name: "key2", Type: model.SecretTypeBearer, Host: "b.example.com", EncryptedValue: []byte(`{"token":"t2"}`)}
	if err := s.CreateSecret(ctx, other); err != nil {
		t.Fatalf("create other secret: %v", err)
	}

	bind := func(secretID string) {
		t.Helper()
		if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: secretID,
		}); err != nil {
			t.Fatalf("upsert binding: %v", err)
		}
	}

	// First bind, then rebind the same env to a different secret; the upsert must
	// replace rather than create a duplicate.
	bind(sec.ID)
	bind(other.ID)
	bindings, err := s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(bindings))
	}
	if bindings[0].SecretID != other.ID {
		t.Fatalf("secret id = %q, want %q", bindings[0].SecretID, other.ID)
	}

	if err := s.DeleteHarnessConfigSecretBinding(ctx, "project-1", config.ID, "OPENAI_API_KEY"); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	bindings, _ = s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if len(bindings) != 0 {
		t.Fatalf("bindings after delete = %d, want 0", len(bindings))
	}
}

func TestDeletingSecretRemovesHarnessConfigBindings(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	if err := s.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	bindings, err := s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings after secret delete = %d, want 0", len(bindings))
	}
}

func TestDeletingSecretRemovesGrants(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := s.CreateSecretGrant(ctx, &model.SecretGrant{
		ProjectID: "project-1", SecretID: sec.ID, Scope: model.SecretGrantScopeProject, ScopeKey: "project-1",
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	if err := s.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	grants, err := s.ListSecretGrants(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants after secret delete = %d, want 0", len(grants))
	}
}

func TestDeletingHarnessConfigRemovesBindings(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeBearer, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	if err := s.DeleteHarnessConfig(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("delete harness config: %v", err)
	}
	bindings, err := s.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings after harness config delete = %d, want 0", len(bindings))
	}
}
