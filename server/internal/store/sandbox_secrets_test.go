package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestDeleteSandboxGCsSecretsAndAnonymous(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	if err := s.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	anon := &model.Secret{
		ID: "anon-1", ProjectID: "project-1", Name: "sandbox-secret-anon-1",
		Type: model.SecretTypeToken, Anonymous: true, UniqueKey: "anon-1",
		EncryptedValue: []byte(`{"token":"x"}`),
	}
	if err := s.CreateSecret(ctx, anon); err != nil {
		t.Fatalf("create anon secret: %v", err)
	}
	referenced := &model.Secret{
		ID: "ref-1", ProjectID: "project-1", Name: "shared",
		Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"y"}`),
	}
	if err := s.CreateSecret(ctx, referenced); err != nil {
		t.Fatalf("create referenced secret: %v", err)
	}

	for _, a := range []*model.SandboxSecret{
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "anon-1", EnvName: "A", Sentinel: "SENT-A"},
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "ref-1", EnvName: "B", Sentinel: "SENT-B"},
	} {
		if err := s.CreateSandboxSecret(ctx, a); err != nil {
			t.Fatalf("create assignment: %v", err)
		}
	}

	if err := s.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	// Assignments are gone.
	if _, err := s.GetSandboxSecretBySentinel(ctx, "sb-1", "SENT-A"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("assignment SENT-A still present: %v", err)
	}
	// Anonymous secret is deleted.
	if _, err := s.GetSecret(ctx, "project-1", "anon-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("anonymous secret not deleted: %v", err)
	}
	// Referenced secret survives.
	if _, err := s.GetSecret(ctx, "project-1", "ref-1"); err != nil {
		t.Fatalf("referenced secret should survive: %v", err)
	}
}
