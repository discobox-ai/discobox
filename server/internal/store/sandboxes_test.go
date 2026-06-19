package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/secrets"
	"github.com/obot-platform/discobox/server/internal/store"
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

func TestGetResourcesByShortIDSuffix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	project := &model.Project{ID: "000000000000000000abc12345", OwnerUserID: "user-1", Name: "Project", Slug: "project-short-id"}
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{ProjectID: project.ID, UserID: "user-1", Role: "owner"}); err != nil {
		t.Fatalf("create project member: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "000000000000000001abc12345", ProjectID: project.ID, Type: "docker", Name: "provider"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	sandbox := &model.Sandbox{ID: "000000000000000002abc12345", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "sandbox"}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	worker := &model.Worker{ID: "000000000000000003abc12345", ProjectID: project.ID, ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	job := &orchestration.Job{ID: "000000000000000004abc12345", Type: "test", Payload: []byte(`{}`), Resource: orchestration.Resource{Type: "sandbox", ID: sandbox.ID}}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	short := "abc12345"
	gotProject, err := s.GetProject(ctx, short)
	if err != nil || gotProject.ID != project.ID {
		t.Fatalf("short project = %#v err=%v", gotProject, err)
	}
	gotProvider, err := s.GetSandboxProviderInstance(ctx, project.ID, short)
	if err != nil || gotProvider.ID != provider.ID {
		t.Fatalf("short provider = %#v err=%v", gotProvider, err)
	}
	gotSandbox, err := s.GetSandbox(ctx, project.ID, short)
	if err != nil || gotSandbox.ID != sandbox.ID {
		t.Fatalf("short sandbox = %#v err=%v", gotSandbox, err)
	}
	gotWorker, err := s.GetWorker(ctx, short)
	if err != nil || gotWorker.ID != worker.ID {
		t.Fatalf("short worker = %#v err=%v", gotWorker, err)
	}
	gotJob, err := s.GetJobForProject(ctx, project.ID, short)
	if err != nil || gotJob.ID != job.ID {
		t.Fatalf("short job = %#v err=%v", gotJob, err)
	}

	ambiguous := &model.Sandbox{ID: "000000000000000005abc12345", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "ambiguous"}
	if err := s.CreateSandbox(ctx, ambiguous); err != nil {
		t.Fatalf("create ambiguous sandbox: %v", err)
	}
	if _, err := s.GetSandbox(ctx, project.ID, short); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ambiguous short sandbox error = %v, want not found", err)
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
	if bytes.Equal(got.SecretState, plaintext) {
		t.Fatalf("loaded secret state equals plaintext")
	}
	if !secrets.IsSealed(got.SecretState) {
		t.Fatalf("loaded secret state is not sealed")
	}
	opened, err := s.OpenSandboxSecretState(ctx, got)
	if err != nil {
		t.Fatalf("open sandbox secret state: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened secret state = %q, want %q", string(opened), string(plaintext))
	}

	sealed := append([]byte(nil), got.SecretState...)
	got.Name = "renamed secret"
	if err := s.UpdateSandbox(ctx, got); err != nil {
		t.Fatalf("update sandbox with sealed secret state: %v", err)
	}
	var updatedRow struct {
		SecretState []byte
	}
	if err := db.Write.WithContext(ctx).
		Model(&model.Sandbox{}).
		Select("secret_state").
		Where("id = ?", sandbox.ID).
		Scan(&updatedRow).Error; err != nil {
		t.Fatalf("read updated raw secret state: %v", err)
	}
	if !bytes.Equal(updatedRow.SecretState, sealed) {
		t.Fatalf("sealed secret state changed on metadata-only update")
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	project := &model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
		Slug:        "project",
	}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return store.New(db.Write, db.Read, store.WithSealer(sealer)), db
}
