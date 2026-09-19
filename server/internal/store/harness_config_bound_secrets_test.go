package store_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// BoundSecrets is not a column, so every read that hands out a harness config
// has to count it — the sandbox's preloaded one included, since that is what
// the listing a client polls carries. A read that skipped the count would say
// a harness with credentials has none.
func TestEveryHarnessConfigReadCountsItsBindings(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")

	bound := &model.HarnessConfig{ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	unbound := &model.HarnessConfig{ProjectID: "project-1", Slug: "claude", Name: "Claude", RunCommand: []string{"claude"}}
	for _, cfg := range []*model.HarnessConfig{bound, unbound} {
		if err := s.CreateHarnessConfig(ctx, cfg); err != nil {
			t.Fatalf("create harness config: %v", err)
		}
	}
	for _, env := range []string{"OPENAI_API_KEY", "CODEX_AUTH"} {
		sec := &model.Secret{ProjectID: "project-1", Name: env, Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}
		if err := s.CreateSecret(ctx, sec); err != nil {
			t.Fatalf("create secret: %v", err)
		}
		if err := s.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID: "project-1", HarnessConfigID: bound.ID, EnvName: env, SecretID: sec.ID,
		}); err != nil {
			t.Fatalf("upsert binding: %v", err)
		}
	}
	if err := s.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
		SandboxManifest: model.SandboxManifest{HarnessConfigID: &bound.ID},
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	want := map[string]int{bound.ID: 2, unbound.ID: 0}
	check := func(read string, cfg *model.HarnessConfig) {
		t.Helper()
		if cfg == nil {
			t.Fatalf("%s: no harness config", read)
		}
		if cfg.BoundSecrets != want[cfg.ID] {
			t.Fatalf("%s: %s BoundSecrets = %d, want %d", read, cfg.Slug, cfg.BoundSecrets, want[cfg.ID])
		}
	}

	configs, err := s.ListHarnessConfigs(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := range configs {
		check("ListHarnessConfigs", &configs[i])
	}
	got, err := s.GetHarnessConfig(ctx, "project-1", bound.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	check("GetHarnessConfig", got)
	if got, err = s.GetHarnessConfigByID(ctx, bound.ID); err != nil {
		t.Fatalf("get by id: %v", err)
	}
	check("GetHarnessConfigByID", got)
	if got, err = s.GetHarnessConfigByName(ctx, "project-1", "Codex"); err != nil {
		t.Fatalf("get by name: %v", err)
	}
	check("GetHarnessConfigByName", got)
	if got, err = s.GetHarnessConfigBySlug(ctx, "project-1", "claude"); err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	check("GetHarnessConfigBySlug", got)

	sandboxes, err := s.ListSandboxes(ctx, "project-1", "", nil)
	if err != nil || len(sandboxes) != 1 {
		t.Fatalf("list sandboxes = %d, %v", len(sandboxes), err)
	}
	check("ListSandboxes", sandboxes[0].HarnessConfig)
	sandbox, err := s.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	check("GetSandbox", sandbox.HarnessConfig)
	if sandbox, err = s.FindSandboxByIDPrefix(ctx, "sb-1"); err != nil {
		t.Fatalf("find sandbox: %v", err)
	}
	check("FindSandboxByIDPrefix", sandbox.HarnessConfig)

	// And a save is not a write of it: the count is the bindings', never a
	// value a stale copy can put back.
	got, _ = s.GetHarnessConfig(ctx, "project-1", unbound.ID)
	got.BoundSecrets = 7
	if err := s.UpdateHarnessConfig(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = s.GetHarnessConfig(ctx, "project-1", unbound.ID); got.BoundSecrets != 0 {
		t.Fatalf("BoundSecrets after a save = %d, want the bindings' 0", got.BoundSecrets)
	}
}
