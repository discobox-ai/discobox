package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A grant scoped to a sandbox or harness config names an ID that is never
// reused, so deleting the one deletes the other: left behind, it covers
// nothing and is still listed as standing. For the same reason none is written
// for one that does not exist.
func TestDeletingAGrantsScopeDeletesTheGrant(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")

	sec := &model.Secret{ProjectID: "project-1", Name: "key", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	if err := s.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	grant := func(scope, key string) error {
		return s.CreateSecretGrant(ctx, &model.SecretGrant{ProjectID: "project-1", SecretID: sec.ID, Scope: scope, ScopeKey: key})
	}
	for _, g := range [][2]string{
		{model.SecretGrantScopeSandbox, "sb-1"},
		{model.SecretGrantScopeHarnessConfig, config.ID},
		{model.SecretGrantScopeProject, "project-1"},
	} {
		if err := grant(g[0], g[1]); err != nil {
			t.Fatalf("create %s grant: %v", g[0], err)
		}
	}
	// Nor is one written for a scope that is not there to begin with.
	if err := grant(model.SecretGrantScopeSandbox, "sb-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant to a missing sandbox = %v, want ErrNotFound", err)
	}
	if err := grant(model.SecretGrantScopeHarnessConfig, "hc-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant to a missing harness config = %v, want ErrNotFound", err)
	}

	if err := s.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if err := s.DeleteHarnessConfig(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("delete harness config: %v", err)
	}

	grants, err := s.ListSecretGrants(ctx, "project-1", "", "")
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].Scope != model.SecretGrantScopeProject {
		t.Fatalf("grants left = %+v, want only the project's", grants)
	}
}
