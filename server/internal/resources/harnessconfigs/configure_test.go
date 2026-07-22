package harnessconfigs

import (
	"bytes"
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

func newTestStore(t *testing.T, opts ...store.Option) *store.Store {
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
	return store.New(db.Write, db.Read, opts...)
}

// The seed tells the configure command which secrets exist without handing it
// any of them: values ride in as PREV_-prefixed sentinels instead. A value in
// this file would be a plaintext credential sitting in the sandbox's filesystem.
func TestPreviousConfigurationCarriesNoSecretValues(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	mine := &model.Secret{ProjectID: "project-1", Name: "configured", Type: model.SecretTypeBearer, Host: "api.example.com", EncryptedValue: []byte(`{"token":"s3cr3t"}`)}
	if err := st.CreateSecret(ctx, mine); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	// A secret the user bound by hand is not the configure flow's to replay.
	theirs := &model.Secret{ProjectID: "project-1", Name: "hand-bound", Type: model.SecretTypeBearer, Host: "other.example.com", EncryptedValue: []byte(`{"token":"theirs"}`)}
	if err := st.CreateSecret(ctx, theirs); err != nil {
		t.Fatalf("create hand-bound secret: %v", err)
	}

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		Configured:          true,
		ConfiguredFiles:     []model.HarnessConfigFile{{Path: "auth.json", Content: "prev"}},
		ConfiguredSecretIDs: []string{mine.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	for env, s := range map[string]*model.Secret{"CONFIGURED_KEY": mine, "HAND_BOUND_KEY": theirs} {
		if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: env, SecretID: s.ID,
		}); err != nil {
			t.Fatalf("bind %s: %v", env, err)
		}
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out, err := svc.previousConfiguration(ctx, config)
	if err != nil {
		t.Fatalf("previousConfiguration: %v", err)
	}

	if len(out.Files) != 1 || out.Files[0].Content != "prev" {
		t.Fatalf("files = %v, want the previous configured files", out.Files)
	}
	if len(out.Secrets) != 1 || out.Secrets[0].EnvName != "CONFIGURED_KEY" {
		t.Fatalf("secrets = %#v, want only the configure-created secret", out.Secrets)
	}
	if !out.Secrets[0].UsePrevious {
		t.Fatalf("secret = %#v, want it marked as reusable", out.Secrets[0])
	}
	seed, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	// The strongest form of this check: no secret material anywhere in the bytes
	// that get written into the sandbox.
	for _, leaked := range []string{"s3cr3t", "theirs", "token"} {
		if bytes.Contains(seed, []byte(leaked)) {
			t.Fatalf("seed leaks secret material %q: %s", leaked, seed)
		}
	}
}

// A configure command that keeps the existing credential must keep the existing
// secret row, binding, and grant — not create a valueless duplicate, and not let
// the replacement sweep delete the credential it just said to keep.
func TestApplyConfigureOutputKeepsPreviousSecret(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeBearer, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Image: "img:1", RunCommand: []string{"claude"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "ANTHROPIC_API_KEY", SecretID: previous.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "ANTHROPIC_API_KEY", Name: "Anthropic API key", Type: "bearer", UsePrevious: true,
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := config.ConfiguredSecretIDs; len(got) != 1 || got[0] != previous.ID {
		t.Fatalf("configured secrets = %v, want the kept secret %s", got, previous.ID)
	}
	if _, err := st.GetSecret(ctx, "project-1", previous.ID); err != nil {
		t.Fatalf("kept secret was deleted by the replacement sweep: %v", err)
	}
}

// Keeping a credential that was never configured is a broken output, not a
// harness configured with nothing.
func TestApplyConfigureOutputRejectsUnbackedReuse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Image: "img:1", RunCommand: []string{"claude"},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{EnvName: "ANTHROPIC_API_KEY", UsePrevious: true}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err == nil {
		t.Fatal("apply succeeded, want a rejection for reusing a secret that does not exist")
	}
}

// A command that passes the PREV_ sentinel back as the value (X=$PREV_X) means
// "keep this one". Storing the sentinel itself would configure the harness with
// a placeholder that resolves to nothing.
func TestApplyConfigureOutputTreatsSentinelPassthroughAsReuse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeBearer, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Image: "img:1", RunCommand: []string{"claude"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	const sentinel = "sentinel-value-for-previous-secret"
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sandbox-1", SecretID: previous.ID,
		EnvName: harness.ConfigurePreviousEnvPrefix + "ANTHROPIC_API_KEY", Sentinel: sentinel,
	}); err != nil {
		t.Fatalf("create sandbox secret: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "ANTHROPIC_API_KEY", Name: "Anthropic API key", Type: "bearer",
		Value: []byte(`{"token":"` + sentinel + `"}`),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := config.ConfiguredSecretIDs; len(got) != 1 || got[0] != previous.ID {
		t.Fatalf("configured secrets = %v, want the kept secret %s", got, previous.ID)
	}
	stored, err := st.GetSecret(ctx, "project-1", previous.ID)
	if err != nil {
		t.Fatalf("get kept secret: %v", err)
	}
	value, err := st.OpenSecretValue(ctx, stored)
	if err != nil {
		t.Fatalf("open kept secret: %v", err)
	}
	if value.Token != "old" {
		t.Fatalf("token = %q, want the original credential rather than the sentinel", value.Token)
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

// Reconfiguring replaces the previous generation of configure-created secrets;
// without the replacement, every reconfigure would orphan one secret.
func TestApplyConfigureOutputReplacesPreviousGeneration(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeBearer, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex",
		Image: "img:1", RunCommand: []string{"codex"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "TOKEN", Name: "new-token", Type: "bearer", Value: []byte(`{"token":"new"}`),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if _, err := st.GetSecret(ctx, "project-1", previous.ID); err == nil {
		t.Fatal("previous generation secret still exists, want replaced")
	}
	if len(config.ConfiguredSecretIDs) != 1 || config.ConfiguredSecretIDs[0] == previous.ID {
		t.Fatalf("configured secrets = %v, want one new id", config.ConfiguredSecretIDs)
	}
	created, err := st.GetSecret(ctx, "project-1", config.ConfiguredSecretIDs[0])
	if err != nil {
		t.Fatalf("get new secret: %v", err)
	}
	// The new row must carry its own unique key so two harnesses can each hold a
	// hostless secret of the same type.
	if created.UniqueKey != created.ID {
		t.Fatalf("unique key = %q, want the secret's own id", created.UniqueKey)
	}
	grant, err := st.FindLiveGrant(ctx, "project-1", created.ID, "", []store.GrantScope{{Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: config.ID}})
	if err != nil || grant == nil {
		t.Fatalf("grant = %v err = %v, want a live harnessConfig grant", grant, err)
	}
}
