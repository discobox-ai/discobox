package harnessconfigs

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

type stubInspector struct {
	byImage map[string]imageMetadata
	calls   []string
}

func (s *stubInspector) Inspect(_ context.Context, image string) (imageMetadata, error) {
	s.calls = append(s.calls, image)
	return s.byImage[image], nil
}

func newReconcileStore(t *testing.T) *store.Store {
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
	return store.New(db.Write, db.Read)
}

func TestReconcileDefinitionImagesRefreshesStaleConfig(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)

	// A definition-backed config pinned to a now-stale dev image.
	stale := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", DefinitionID: "codex",
		Image: "discobox-harness-codex:dev-old", ImageDigest: "sha256:old", RunCommand: []string{"old"},
	}
	if err := st.CreateHarnessConfig(ctx, stale); err != nil {
		t.Fatalf("create stale config: %v", err)
	}
	// A custom (non-definition) config must be left untouched.
	custom := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "custom", Name: "Custom",
		Image: "example.com/custom:local", RunCommand: []string{"custom"},
	}
	if err := st.CreateHarnessConfig(ctx, custom); err != nil {
		t.Fatalf("create custom config: %v", err)
	}

	const newImage = "discobox-harness-codex:dev-new"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		newImage: {Digest: "sha256:new", Harness: harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--new"},
			Secrets: []harness.Secret{{Name: "OPENAI_API_KEY", Required: true}},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": newImage}}

	if err := svc.ReconcileDefinitionImages(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, err := st.GetHarnessConfig(ctx, "project-1", stale.ID)
	if err != nil {
		t.Fatalf("get refreshed config: %v", err)
	}
	if got.Image != newImage {
		t.Fatalf("image = %q, want %q", got.Image, newImage)
	}
	if got.ImageDigest != "sha256:new" {
		t.Fatalf("digest = %q, want sha256:new", got.ImageDigest)
	}
	if len(got.RunCommand) != 2 || got.RunCommand[0] != "codex" || got.RunCommand[1] != "--new" {
		t.Fatalf("runCommand = %v, want [codex --new]", got.RunCommand)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "OPENAI_API_KEY" {
		t.Fatalf("secrets = %v, want OPENAI_API_KEY", got.Secrets)
	}

	// The custom config keeps its explicit image and is never inspected.
	gotCustom, err := st.GetHarnessConfig(ctx, "project-1", custom.ID)
	if err != nil {
		t.Fatalf("get custom config: %v", err)
	}
	if gotCustom.Image != "example.com/custom:local" {
		t.Fatalf("custom image = %q, want unchanged", gotCustom.Image)
	}
	if len(inspector.calls) != 1 || inspector.calls[0] != newImage {
		t.Fatalf("inspector calls = %v, want [%s]", inspector.calls, newImage)
	}
}

func TestReconcileDefinitionImagesSkipsWhenAlreadyCurrent(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)

	const image = "discobox-harness-codex:dev-current"
	current := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", DefinitionID: "codex",
		Image: image, ImageDigest: "sha256:current", RunCommand: []string{"codex"},
	}
	if err := st.CreateHarnessConfig(ctx, current); err != nil {
		t.Fatalf("create config: %v", err)
	}

	inspector := &stubInspector{}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.ReconcileDefinitionImages(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(inspector.calls) != 0 {
		t.Fatalf("inspector calls = %v, want none", inspector.calls)
	}
}

func TestReconcileDefinitionImagesNoOverrides(t *testing.T) {
	ctx := context.Background()
	st := newReconcileStore(t)
	inspector := &stubInspector{}
	svc := &Service{store: st, inspector: inspector}
	if err := svc.ReconcileDefinitionImages(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(inspector.calls) != 0 {
		t.Fatalf("inspector calls = %v, want none", inspector.calls)
	}
}
