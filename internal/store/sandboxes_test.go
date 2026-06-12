package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/internal/database"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/secrets"
	"github.com/obot-platform/discobox/internal/store"
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

func TestSandboxSecretStateEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	s, db := newTestStoreWithDB(t, sealer)
	plaintext := []byte("provider secret state")
	sandbox := &model.Sandbox{
		ID:              "sandbox-secret",
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		Name:            "secret",
		SecretState:     plaintext,
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	var row struct {
		SecretState []byte
	}
	if err := db.Write.WithContext(ctx).
		Model(&model.Sandbox{}).
		Select("secret_state").
		Where("id = ?", sandbox.ID).
		Scan(&row).Error; err != nil {
		t.Fatalf("read raw secret state: %v", err)
	}
	if bytes.Equal(row.SecretState, plaintext) {
		t.Fatalf("raw secret state equals plaintext")
	}

	got, err := s.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !bytes.Equal(got.SecretState, plaintext) {
		t.Fatalf("decrypted secret state = %q, want %q", string(got.SecretState), string(plaintext))
	}
}

func newTestStore(t *testing.T) *store.Store {
	s, _ := newTestStoreWithDB(t, nil)
	return s
}

func newTestStoreWithDB(t *testing.T, sealer secrets.Sealer) (*store.Store, *database.DB) {
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
	if err := db.MigrateTenant(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	project := &model.Project{
		ID:          "project-1",
		TenantID:    "tenant-1",
		OwnerUserID: "user-1",
		Name:        "Project",
		Slug:        "project",
	}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return store.New(database.StaticResolver{DB: db}, store.WithSealer(sealer), store.WithDefaultTenantID("tenant-1")), db
}
