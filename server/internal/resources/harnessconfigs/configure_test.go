package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
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
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
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

	mine := &model.Secret{ProjectID: "project-1", Name: "configured", Type: model.SecretTypeToken, Host: "api.example.com", EncryptedValue: []byte(`{"token":"s3cr3t"}`)}
	if err := st.CreateSecret(ctx, mine); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	// A secret the user bound by hand is not the configure flow's to replay.
	theirs := &model.Secret{ProjectID: "project-1", Name: "hand-bound", Type: model.SecretTypeToken, Host: "other.example.com", EncryptedValue: []byte(`{"token":"theirs"}`)}
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
	// that get written into the sandbox. The value's field name rather than the
	// bare word, since "token" is also a secret *type* and says nothing about
	// what the credential is.
	for _, leaked := range []string{"s3cr3t", "theirs", `"token":`} {
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
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
	configured := &model.Secret{ProjectID: "project-1", Name: "from-configure", Type: model.SecretTypeToken, Host: "configure.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, configured); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	unrelated := &model.Secret{ProjectID: "project-1", Name: "user-owned", Type: model.SecretTypeToken, Host: "user.example.com", EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, unrelated); err != nil {
		t.Fatalf("create unrelated secret: %v", err)
	}

	baselineFiles := []model.HarnessConfigFile{{Path: "baseline.json", Content: "{}"}}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		ConfigCommand:       []string{"/usr/local/libexec/discobox/configure-codex"},
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
		newImage: {Digest: "sha256:new", ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--new"},
			Config: &harness.ImageMode{Command: []string{"configure"}, Reminder: "Log in, then exit."},
		}}},
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
	if got.ConfigReminder != "Log in, then exit." {
		t.Fatalf("config reminder = %q, want re-snapshotted image guidance", got.ConfigReminder)
	}
	// Re-imaging must not silently unconfigure a working harness.
	if !got.Configured || len(got.ConfiguredFiles) != 1 {
		t.Fatalf("configured=%t files=%v, want configure output preserved", got.Configured, got.ConfiguredFiles)
	}
}

// A stable tag is rebuilt in place, so seeding re-inspects on every pass and
// compares digests. Skipping the inspect when the reference matched — the
// previous behavior — left ImageDigest pinned to a build that no longer existed
// under that tag, which would report every sandbox as current forever
// (ADR 0016 §7).
func TestSeedBuiltInsRefreshesDigestForUnchangedImageReference(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const image = "discobox-harness-codex:dev-current"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: image, ImageDigest: "sha256:old", RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	rebuilt := imageMetadata{Digest: "sha256:rebuilt", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--rebuilt"}},
	}}
	inspector := &stubInspector{byImage: map[string]imageMetadata{image: rebuilt}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ImageDigest != "sha256:rebuilt" {
		t.Fatalf("imageDigest = %q, want the rebuilt digest under the same tag", got.ImageDigest)
	}
	if len(got.RunCommand) != 2 || got.RunCommand[1] != "--rebuilt" {
		t.Fatalf("runCommand = %v, want re-snapshotted from the rebuilt label", got.RunCommand)
	}
}

// Seeding still writes nothing when neither the reference nor the digest moved.
func TestSeedBuiltInsSkipsWriteForUnchangedDigest(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const image = "discobox-harness-codex:dev-current"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: image, ImageDigest: "sha256:same", RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatalf("create config: %v", err)
	}
	before, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	unchanged := imageMetadata{Digest: "sha256:same", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--clobbered"}},
	}}
	inspector := &stubInspector{byImage: map[string]imageMetadata{image: unchanged}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) || len(got.RunCommand) != 1 {
		t.Fatalf("config rewritten (runCommand = %v); an unchanged digest must not write", got.RunCommand)
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
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

// A reconfigure that returns a new value for an env name the harness already
// binds updates the existing secret in place: the secret ID (and thus every
// sandbox sentinel keyed on it) is stable, and the standing grant follows a
// host change so the value keeps resolving.
func TestApplyConfigureOutputUpdatesBoundSecretInPlace(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, Host: "old.example.com", UniqueKey: "old", EncryptedValue: []byte(`{"token":"old"}`)}
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
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "TOKEN", SecretID: previous.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := st.CreateSecretGrant(ctx, &model.SecretGrant{
		ProjectID: "project-1", SecretID: previous.ID,
		Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: config.ID, Host: "old.example.com",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "TOKEN", Name: "new-token", Type: "bearer", Host: "new.example.com", Value: []byte(`{"token":"new"}`),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Same ID, updated value and host — no new row.
	if got := config.ConfiguredSecretIDs; len(got) != 1 || got[0] != previous.ID {
		t.Fatalf("configured secrets = %v, want the same id %s updated in place", got, previous.ID)
	}
	updated, err := st.GetSecret(ctx, "project-1", previous.ID)
	if err != nil {
		t.Fatalf("get updated secret: %v", err)
	}
	if string(updated.EncryptedValue) != `{"token":"new"}` {
		t.Fatalf("value = %q, want the new value", updated.EncryptedValue)
	}
	if updated.Host != "new.example.com" {
		t.Fatalf("host = %q, want new.example.com", updated.Host)
	}
	// The grant must have followed the host, or the value stops resolving for it.
	grant, err := st.FindLiveGrant(ctx, "project-1", previous.ID, "new.example.com", []store.GrantScope{{Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: config.ID}})
	if err != nil || grant == nil {
		t.Fatalf("grant = %v err = %v, want a live grant for the new host", grant, err)
	}
	if grant.Host != "new.example.com" {
		t.Fatalf("grant host = %q, want new.example.com", grant.Host)
	}
}

// `shell` is seeded from the registry like any other harness — same inspect,
// same snapshot — and what it declares is what it gets. The `docker` group is
// why that matters: the image ships the Docker CLI, which checks group
// membership, so without it Docker works as the sandbox user under a coding
// harness and not under a plain shell.
//
// It carries no run command, which is the declaration that the sandbox resolves
// the login shell, and it is born configured because it collects no credentials
// — derived from the empty secret list, not from its slug (ADR 0043).
func TestSeedBuiltInsSeedsShellLikeAnyOtherHarness(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const image = "discobox-harness-shell:local"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		image: {Digest: "sha256:shell", ImageMetadata: harness.ImageMetadata{
			APIVersion:       harness.ImageAPIVersion,
			AdditionalGroups: []string{"docker"},
			Harness:          &harness.Image{ID: "shell", Name: "Shell"},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"shell": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "shell")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(got.AdditionalGroups, []string{"docker"}) {
		t.Fatalf("additionalGroups = %v, want [docker] so a non-root shell can use Docker", got.AdditionalGroups)
	}
	if got.Image != image || got.ImageDigest != "sha256:shell" {
		t.Fatalf("image = %q digest = %q, want the inspected identity", got.Image, got.ImageDigest)
	}
	if len(got.RunCommand) != 0 {
		t.Fatalf("runCommand = %v, want none so the sandbox resolves the login shell", got.RunCommand)
	}
	if !got.Configured || !got.BuiltIn {
		t.Fatalf("configured = %t builtIn = %t, want both true", got.Configured, got.BuiltIn)
	}
}

// The flip side: a harness that does declare secrets is seeded unconfigured, so
// deriving Configured from "collects nothing" cannot make a credential-hungry
// harness look ready to run.
func TestSeedBuiltInsLeavesHarnessesWithSecretsUnconfigured(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const image = "discobox-harness-codex:local"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		image: {Digest: "sha256:codex", ImageMetadata: harness.ImageMetadata{
			Harness: &harness.Image{
				ID: "codex", Name: "Codex", RunCommand: []string{"codex"},
				Secrets: []harness.Secret{{Name: "OPENAI_API_KEY", Required: true}},
			},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Configured {
		t.Fatal("configured = true, want false for a harness that still needs a credential")
	}
}

// A harness with nothing to configure cannot be turned off, because configure
// is what would turn it back on and configure refuses it too. Without this the
// reserved `shell` built-in is one keystroke from being permanently unusable:
// unconfigured harnesses are rejected at sandbox create, a built-in cannot be
// deleted, and seeding never revisits Configured.
func TestDeconfigureIsRefusedWhenThereIsNothingToConfigure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "shell", Name: "Shell", BuiltIn: true,
		Image: "discobox-harness-shell:local",
		// No ConfigCommand and no secrets: born configured, with no setup to run.
		Configured: true,
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	if _, err := svc.DeconfigureHarnessConfig(ctx, "project-1", config.ID); err == nil {
		t.Fatal("deconfiguring a harness with nothing to configure should be refused")
	}

	after, err := st.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("get harness config: %v", err)
	}
	if !after.Configured {
		t.Fatal("the refused deconfigure must leave the harness configured")
	}
}

// The refusal to delete a built-in points at deconfigure only where that would
// do something. Sending someone to a command that answers "already off", or
// that refuses them in turn, is worse than saying nothing.
func TestBuiltInDeleteHintOnlySuggestsWhatWouldWork(t *testing.T) {
	configurable := []string{"/usr/local/libexec/discobox/configure-codex"}
	for _, tc := range []struct {
		name   string
		config *model.HarnessConfig
		want   string
	}{
		{
			name:   "on and configurable, so it can be turned off",
			config: &model.HarnessConfig{Slug: "codex", Configured: true, ConfigCommand: configurable},
			want:   "; run `discobox admin harness deconfigure codex` to turn it off",
		},
		{
			name:   "already off",
			config: &model.HarnessConfig{Slug: "opencode", Configured: false, ConfigCommand: configurable},
			want:   "; it is already off",
		},
		{
			name:   "nothing to configure, so it cannot be turned off at all",
			config: &model.HarnessConfig{Slug: "shell", Configured: true},
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := builtInDeleteHint(tc.config); got != tc.want {
				t.Fatalf("hint = %q, want %q", got, tc.want)
			}
		})
	}
}

// stubSandboxRuntime records the create body the configure flow builds. Only
// CreateSandbox is exercised; the rest satisfies the interface.
type stubSandboxRuntime struct {
	created services.CreateSandboxBody
	rebound []string
}

func (s *stubSandboxRuntime) RebindHarnessConfigSecrets(_ context.Context, _, harnessConfigID string) error {
	s.rebound = append(s.rebound, harnessConfigID)
	return nil
}

func (s *stubSandboxRuntime) CreateSandbox(_ context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	s.created = input
	return &model.Sandbox{ID: "sandbox-1", ProjectID: projectID}, nil
}

func (s *stubSandboxRuntime) DeleteSandbox(context.Context, string, string) error { return nil }

func (s *stubSandboxRuntime) AcquireSandboxHTTPClient(context.Context, string, string, []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	return nil, nil, errors.New("not used")
}

type stubDirtier struct{}

func (stubDirtier) MarkDirtyAt(context.Context, string, string, time.Time) error { return nil }

// The configure sandbox names its own account: it has no source and no caller
// identity to mirror, and root is the one identity it must not use. A harness
// CLI may refuse to run as root -- Claude Code refuses bypassPermissions there
// -- so configuring as root would collect a credential under an identity no run
// sandbox ever uses.
func TestConfigureSandboxRunsAsNonRootUser(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude", Name: "Claude",
		Image:         "discobox-harness-claude-code:local",
		ConfigCommand: []string{"/usr/local/libexec/discobox/configure-claude-code"},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}

	runtime := &stubSandboxRuntime{}
	svc := &Service{store: st, inspector: &stubInspector{}, sandboxes: runtime, dirtier: stubDirtier{}}
	if _, err := svc.ConfigureHarnessConfig(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("configure: %v", err)
	}

	user, ok := runtime.created.Config.User.Get()
	if !ok {
		t.Fatal("configure sandbox requested no user, so it would run as the image's root")
	}
	if uid, ok := user.UID.Get(); !ok || uid == 0 {
		t.Fatalf("configure sandbox uid = %v (set=%v), want a non-root uid", uid, ok)
	}
	if uid, _ := user.UID.Get(); uid != harness.ConfigureUserUID {
		t.Fatalf("configure sandbox uid = %d, want %d", uid, harness.ConfigureUserUID)
	}
	if gid, _ := user.Gid.Get(); gid != harness.ConfigureUserGID {
		t.Fatalf("configure sandbox gid = %d, want %d", gid, harness.ConfigureUserGID)
	}
	// boot creates an account the image does not have, and requires a name to
	// create it under (ADR 0025 §4).
	if name, _ := user.Name.Get(); name != harness.ConfigureUserName {
		t.Fatalf("configure sandbox user name = %q, want %q", name, harness.ConfigureUserName)
	}
}

// A configure-created credential is the harness's own: its grant never expires,
// so nothing may cap the grants that stand on it. The limit is stated at
// creation rather than left to a default, which under a ceiling would describe
// a lifetime this credential's grant does not have.
func TestConfigureCreatedSecretsHaveNoGrantLimit(t *testing.T) {
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
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "ANTHROPIC_API_KEY", Name: "Anthropic API key", Type: "token", Value: json.RawMessage(`{"token":"sk-ant-x"}`),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(config.ConfiguredSecretIDs) != 1 {
		t.Fatalf("configured secrets = %v, want the one the flow created", config.ConfiguredSecretIDs)
	}

	secret, err := st.GetSecret(ctx, "project-1", config.ConfiguredSecretIDs[0])
	if err != nil {
		t.Fatalf("read the configured secret: %v", err)
	}
	if secret.MaxGrantTTL != 0 {
		t.Fatalf("grant limit = %d, want none: the harness's own grant never expires", secret.MaxGrantTTL)
	}

	// And the grant it stands on says the same thing from the other side.
	grants, err := st.ListSecretGrants(ctx, "project-1", secret.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want the one the configure flow minted", len(grants))
	}
	if grants[0].ExpiresAt != nil {
		t.Fatalf("expires at = %v, want a grant that outlives the configure run", grants[0].ExpiresAt)
	}
}
