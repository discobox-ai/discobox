package harnessconfigs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

func newTestStore(t *testing.T) *store.Store {
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

// The previous configuration is seeded back in the same shape the configure
// command writes, including secret values so it can validate them rather than
// re-prompt — but only for secrets still holding a live grant to this harness
// config. A revoked grant must drop the value out of the seed.
func TestPreviousConfigurationOnlyIncludesGrantedSecrets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	granted := &model.Secret{ProjectID: "project-1", Name: "granted", Type: model.SecretTypeBearer, Host: "granted.example.com", EncryptedValue: []byte(`{"token":"keep"}`)}
	if err := st.CreateSecret(ctx, granted); err != nil {
		t.Fatalf("create granted secret: %v", err)
	}
	revoked := &model.Secret{ProjectID: "project-1", Name: "revoked", Type: model.SecretTypeBearer, Host: "revoked.example.com", EncryptedValue: []byte(`{"token":"drop"}`)}
	if err := st.CreateSecret(ctx, revoked); err != nil {
		t.Fatalf("create revoked secret: %v", err)
	}

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		Configured:          true,
		ConfiguredFiles:     []model.HarnessConfigFile{{Path: "auth.json", Content: "prev"}},
		ConfiguredSecretIDs: []string{granted.ID, revoked.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	for _, s := range []*model.Secret{granted, revoked} {
		if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "KEY_" + s.Name, SecretID: s.ID,
		}); err != nil {
			t.Fatalf("bind %s: %v", s.Name, err)
		}
	}
	// Only the first secret is granted to this harness config.
	if err := st.CreateSecretGrant(ctx, &model.SecretGrant{
		ProjectID: "project-1", SecretID: granted.ID,
		Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: config.ID, Host: granted.Host,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out, err := svc.previousConfiguration(ctx, config)
	if err != nil {
		t.Fatalf("previousConfiguration: %v", err)
	}

	if len(out.Files) != 1 || out.Files[0].Content != "prev" {
		t.Fatalf("files = %v, want the previous configured files", out.Files)
	}
	if len(out.Secrets) != 1 {
		t.Fatalf("secrets = %#v, want only the granted secret", out.Secrets)
	}
	if out.Secrets[0].Name != "granted" || string(out.Secrets[0].Value) != `{"token":"keep"}` {
		t.Fatalf("secret = %#v, want the granted secret with its value", out.Secrets[0])
	}
	// The seed round-trips as the configure output shape.
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
}

// Deconfigure must remove exactly what the configure flow created — its secrets,
// their bindings, and its files — while leaving the image-declared baseline
// (Files/Secrets/RunCommand) intact so the harness can be configured again.
func TestDeconfigureRemovesOnlyConfigureCreatedAssets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// A secret the configure flow created, and one the user owns independently.
	configured := &model.Secret{ProjectID: "project-1", Name: "from-configure", Type: model.SecretTypeBearer, Host: "configure.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, configured); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	unrelated := &model.Secret{ProjectID: "project-1", Name: "user-owned", Type: model.SecretTypeBearer, Host: "user.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, unrelated); err != nil {
		t.Fatalf("create unrelated secret: %v", err)
	}

	baselineFiles := []model.HarnessConfigFile{{Path: "baseline.json", Content: "{}"}}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		Files:               baselineFiles,
		Configured:          true,
		ConfiguredFiles:     []model.HarnessConfigFile{{Path: "auth.json", Content: "secret"}},
		ConfiguredSecretIDs: []string{configured.ID},
		ConfigureError:      "",
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: configured.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	got, err := svc.DeconfigureHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("deconfigure: %v", err)
	}

	if got.Configured {
		t.Fatal("configured = true, want false after deconfigure")
	}
	if len(got.ConfiguredFiles) != 0 || len(got.ConfiguredSecretIDs) != 0 {
		t.Fatalf("configure output not cleared: files=%v secrets=%v", got.ConfiguredFiles, got.ConfiguredSecretIDs)
	}
	// The image-declared baseline survives.
	if len(got.Files) != 1 || got.Files[0].Path != "baseline.json" {
		t.Fatalf("baseline files = %v, want preserved", got.Files)
	}
	if len(got.RunCommand) != 1 || got.RunCommand[0] != "codex" {
		t.Fatalf("runCommand = %v, want preserved", got.RunCommand)
	}
	// The configure-created secret is gone; the user's own secret is untouched.
	if _, err := st.GetSecret(ctx, "project-1", configured.ID); err == nil {
		t.Fatal("configure-created secret still exists, want deleted")
	}
	if _, err := st.GetSecret(ctx, "project-1", unrelated.ID); err != nil {
		t.Fatalf("unrelated secret was removed: %v", err)
	}
	bindings, err := st.ListHarnessConfigSecretBindings(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %v, want none after deconfigure", bindings)
	}
}

// Built-ins track their image: a dev rebuild changes the tag, and seeding
// clobbers it and re-snapshots the label without disturbing Configured.
func TestSeedBuiltInsClobbersImageAndKeepsConfigured(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const newImage = "discobox-harness-codex:dev-new"
	existing := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:dev-old", ImageDigest: "sha256:old",
		RunCommand: []string{"old"}, Configured: true,
		ConfiguredFiles: []model.HarnessConfigFile{{Path: "auth.json", Content: "secret"}},
	}
	if err := st.CreateHarnessConfig(ctx, existing); err != nil {
		t.Fatalf("create config: %v", err)
	}

	inspector := &stubInspector{byImage: map[string]imageMetadata{
		newImage: {Digest: "sha256:new", Harness: harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--new"},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": newImage}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Image != newImage || got.ImageDigest != "sha256:new" {
		t.Fatalf("image = %q digest = %q, want clobbered to the new image", got.Image, got.ImageDigest)
	}
	if len(got.RunCommand) != 2 || got.RunCommand[1] != "--new" {
		t.Fatalf("runCommand = %v, want re-snapshotted from the new label", got.RunCommand)
	}
	// Re-imaging must not silently unconfigure a working harness.
	if !got.Configured || len(got.ConfiguredFiles) != 1 {
		t.Fatalf("configured=%t files=%v, want configure output preserved", got.Configured, got.ConfiguredFiles)
	}
}

// Seeding is idempotent: an unchanged image is not re-inspected or rewritten.
func TestSeedBuiltInsSkipsUnchangedImage(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const image = "discobox-harness-codex:dev-current"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: image, RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	inspector := &stubInspector{}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, called := range inspector.calls {
		if called == image {
			t.Fatalf("re-inspected unchanged image %q", image)
		}
	}
}

// A built-in cannot be deleted: the server seeds it again on the next start, so
// deleting one would silently come back. Deconfigure is the off switch.
func TestDeleteBuiltInHarnessIsRefused(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	builtIn := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:local", RunCommand: []string{"codex"},
	}
	if err := st.CreateHarnessConfig(ctx, builtIn); err != nil {
		t.Fatalf("create built-in: %v", err)
	}
	custom := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "custom", Name: "Custom",
		Image: "example.com/custom:1", RunCommand: []string{"custom"},
	}
	if err := st.CreateHarnessConfig(ctx, custom); err != nil {
		t.Fatalf("create custom: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	err := svc.DeleteHarnessConfig(ctx, "project-1", builtIn.ID)
	if err == nil {
		t.Fatal("deleting a built-in harness succeeded, want refusal")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusConflict {
		t.Fatalf("err = %v, want 409", err)
	}
	if _, err := st.GetHarnessConfig(ctx, "project-1", builtIn.ID); err != nil {
		t.Fatalf("built-in was removed despite the refusal: %v", err)
	}

	// A user-registered harness is still deletable.
	if err := svc.DeleteHarnessConfig(ctx, "project-1", custom.ID); err != nil {
		t.Fatalf("delete custom harness: %v", err)
	}
}
