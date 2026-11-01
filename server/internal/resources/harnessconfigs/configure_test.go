package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// stubInspector answers what byImage holds, by reference: the manifest the
// entry holds as that bundle's own label, digested as the entry is.
// The platform bundle is stubPlatformLayer unless byImage names it.
type stubInspector struct {
	byImage map[string]imageMetadata
	// platformEnv is env the platform bundle's layer declares.
	platformEnv map[string]string
}

func (s *stubInspector) InspectBundle(_ context.Context, bundle string, admit func(map[string]string) error) (bundleInspection, error) {
	inspected, ok := s.byImage[bundle]
	read := stubBundle(inspected.BundleDigest, inspected.ImageMetadata)
	if !ok && bundle == testPlatformBundle {
		read = stubPlatformBundle(s.platformEnv)
	}
	if err := admit(read.Labels); err != nil {
		return bundleInspection{}, err
	}
	return read, nil
}

// admitAny admits every bundle, for a test of what an inspector reads rather
// than of what is admitted.
func admitAny(map[string]string) error { return nil }

// stubBundle is a harness bundle declaring metadata as its own label, on the
// platform's API version.
func stubBundle(digest string, metadata harness.ImageMetadata) bundleInspection {
	metadata.ImageKind, metadata.PlatformAPIVersion = platform.ImageKind{}, harness.PlatformAPIVersion
	label, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return bundleInspection{Digest: digest, Labels: map[string]string{harness.ImageLabel: string(label)}}
}

// testPlatformBundle is the platform bundle a test service composes on, which
// stubInspector answers with stubPlatformBundle.
const testPlatformBundle = "discobox-platform:test"

// testReferences names the platform bundle and no built-in override.
var testReferences = harnessdefs.References{PlatformBundle: testPlatformBundle}

// bundleReferences names each built-in's bundle, which stubInspector answers
// for, on testPlatformBundle.
func bundleReferences(refs map[string]string) harnessdefs.References {
	return harnessdefs.References{PlatformBundle: testPlatformBundle, Bundles: refs}
}

// stubPlatformBundle is a platform bundle whose layer provides the platform
// API version and nothing else.
func stubPlatformBundle(env map[string]string) bundleInspection {
	layer, err := json.Marshal(map[string]any{"apiVersion": "discobox.dev/image/v1", "platformAPIVersion": harness.PlatformAPIVersion, "env": env})
	if err != nil {
		panic(err)
	}
	return bundleInspection{Digest: "sha256:platform", Labels: map[string]string{
		harness.ImageLayerLabelPrefix + harness.PlatformLayer: string(layer),
	}}
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

	mine := &model.Secret{ProjectID: "project-1", Name: "configured", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"api.example.com"}}}, EncryptedValue: []byte(`{"token":"s3cr3t"}`)}
	if err := st.CreateSecret(ctx, mine); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	// A secret the user bound by hand is not the configure flow's to replay.
	theirs := &model.Secret{ProjectID: "project-1", Name: "hand-bound", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"other.example.com"}}}, EncryptedValue: []byte(`{"token":"theirs"}`)}
	if err := st.CreateSecret(ctx, theirs); err != nil {
		t.Fatalf("create hand-bound secret: %v", err)
	}

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		Configured:          true,
		ConfiguredFiles:     []model.HarnessConfigFile{{Path: "auth.json", Content: "prev"}},
		ConfiguredSecretIDs: []string{mine.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	for env, s := range map[string]*model.Secret{"CONFIGURED_KEY": mine, "HAND_BOUND_KEY": theirs} {
		if err := putHarnessBinding(ctx, st, harnessBinding{
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Bundle: "img:1", RunCommand: []string{"claude"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := putHarnessBinding(ctx, st, harnessBinding{
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
		Bundle: "img:1", RunCommand: []string{"claude"},
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Bundle: "img:1", RunCommand: []string{"claude"},
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
	configured := &model.Secret{ProjectID: "project-1", Name: "from-configure", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"configure.example.com"}}}, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, configured); err != nil {
		t.Fatalf("create configured secret: %v", err)
	}
	unrelated := &model.Secret{ProjectID: "project-1", Name: "user-owned", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"user.example.com"}}}, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, unrelated); err != nil {
		t.Fatalf("create unrelated secret: %v", err)
	}

	baselineFiles := []model.HarnessConfigFile{{Path: "baseline.json", Content: "{}"}}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:local", RunCommand: []string{"codex"},
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
	if err := putHarnessBinding(ctx, st, harnessBinding{
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
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %v, want none after deconfigure", bindings)
	}
}

// Built-ins track their bundle: a dev rebuild changes the tag, and seeding
// clobbers it and re-snapshots the label without disturbing Configured.
func TestSeedBuiltInsClobbersBundleAndKeepsConfigured(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const newBundle = "discobox-harness-codex:dev-new"
	existing := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:dev-old", BundleDigest: "sha256:old",
		RunCommand: []string{"old"}, Configured: true,
		ConfiguredFiles: []model.HarnessConfigFile{{Path: "auth.json", Content: "secret"}},
	}
	if err := st.CreateHarnessConfig(ctx, existing); err != nil {
		t.Fatalf("create config: %v", err)
	}

	inspector := &stubInspector{byImage: map[string]imageMetadata{
		newBundle: {BundleDigest: "sha256:new", ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--new"},
			Config: &harness.ImageMode{Command: []string{"configure"}, Reminder: "Log in, then exit."},
		}}},
	}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"codex": newBundle})}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Bundle != newBundle || got.BundleDigest != "sha256:new" {
		t.Fatalf("bundle = %q digest = %q, want clobbered to the new bundle", got.Bundle, got.BundleDigest)
	}
	if len(got.RunCommand) != 2 || got.RunCommand[1] != "--new" {
		t.Fatalf("runCommand = %v, want re-snapshotted from the new label", got.RunCommand)
	}
	if got.ConfigReminder != "Log in, then exit." {
		t.Fatalf("config reminder = %q, want re-snapshotted bundle guidance", got.ConfigReminder)
	}
	// Rebuilding must not silently unconfigure a working harness.
	if !got.Configured || len(got.ConfiguredFiles) != 1 {
		t.Fatalf("configured=%t files=%v, want configure output preserved", got.Configured, got.ConfiguredFiles)
	}
}

// A stable tag is rebuilt in place, so seeding re-inspects on every pass and
// compares digests. Skipping the inspect when the reference matched — the
// previous behavior — left BundleDigest pinned to a build that no longer
// existed under that tag, which would report every sandbox as current forever
// (ADR 0016 §7).
func TestSeedBuiltInsRefreshesDigestForUnchangedBundleReference(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const bundle = "discobox-harness-codex:dev-current"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: bundle, BundleDigest: "sha256:old", RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	rebuilt := imageMetadata{BundleDigest: "sha256:rebuilt", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--rebuilt"}},
	}}
	inspector := &stubInspector{byImage: map[string]imageMetadata{bundle: rebuilt}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"codex": bundle})}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BundleDigest != "sha256:rebuilt" {
		t.Fatalf("bundleDigest = %q, want the rebuilt digest under the same tag", got.BundleDigest)
	}
	if len(got.RunCommand) != 2 || got.RunCommand[1] != "--rebuilt" {
		t.Fatalf("runCommand = %v, want re-snapshotted from the rebuilt label", got.RunCommand)
	}
}

// Seeding writes nothing when neither the references, the digests nor the
// manifest composed from them moved.
func TestSeedBuiltInsSkipsWriteForUnchangedDigest(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const bundle = "discobox-harness-codex:dev-current"
	unchanged := imageMetadata{BundleDigest: "sha256:same", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex"}},
	}}
	inspector := &stubInspector{byImage: map[string]imageMetadata{bundle: unchanged}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"codex": bundle})}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	before, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.ManifestDigest == "" {
		t.Fatal("the seed recorded no manifest digest")
	}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("config rewritten; an unchanged bundle and manifest must not write")
	}
}

// A platform-layer edit moves no harness bundle's digest, and still reaches
// every harness composed on it: the manifest composed from the two moves, so
// the built-in is re-snapshotted and its stopped sandboxes re-pinned.
func TestSeedBuiltInsFollowsAMovedPlatformLayer(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const bundle = "discobox-harness-codex:dev-current"
	inspector := &stubInspector{byImage: map[string]imageMetadata{bundle: {BundleDigest: "sha256:same", ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex"}},
	}}}}
	recorder := &stubSandboxRuntime{}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"codex": bundle}), sandboxes: recorder}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	before, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// The platform's layer gains an env var; the harness bundle is the same.
	inspector.platformEnv = map[string]string{"PLATFORM_EDIT": "1"}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BundleDigest != before.BundleDigest || got.ManifestDigest == before.ManifestDigest || got.Env["PLATFORM_EDIT"] != "1" {
		t.Fatalf("config = bundle %s manifest %s env %v; want the same bundle, a new manifest, and the platform's env", got.BundleDigest, got.ManifestDigest, got.Env)
	}
	if len(recorder.upgraded) != 1 || recorder.upgraded[0] != got.ID {
		t.Fatalf("upgraded = %v, want the config's stopped sandboxes re-pinned once", recorder.upgraded)
	}
}

// A built-in cannot be deleted: the server seeds it again on the next start, so
// deleting one would silently come back. Deconfigure is the off switch.
func TestDeleteBuiltInHarnessIsRefused(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	builtIn := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:local", RunCommand: []string{"codex"},
	}
	if err := st.CreateHarnessConfig(ctx, builtIn); err != nil {
		t.Fatalf("create built-in: %v", err)
	}
	custom := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "custom", Name: "Custom",
		Bundle: "example.com/custom:1", RunCommand: []string{"custom"},
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

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex",
		Bundle: "img:1", RunCommand: []string{"codex"},
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
	// With no host and no routes declared, it may go anywhere (ADR
	// 26-10-27-939 §2), and the harness config binds it for use.
	if !reflect.DeepEqual(created.Routes, []model.SecretRoute{{Hosts: []string{"*"}}}) {
		t.Fatalf("routes = %+v, want *", created.Routes)
	}
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil || len(bindings) != 1 || bindings[0].SecretID != created.ID || bindings[0].EnvName != "TOKEN" || bindings[0].ExpiresAt != nil {
		t.Fatalf("bindings = %+v, %v; want the harness config's use binding of the new secret", bindings, err)
	}
}

// A reconfigure that returns a new value for an env name the harness already
// binds updates the existing secret in place: the secret ID (and thus every
// sandbox sentinel keyed on it) is stable, and the routes it has — which a
// person may have narrowed — are kept: a configure box's output never widens
// them. Routes the image declares are written.
func TestApplyConfigureOutputUpdatesBoundSecretInPlace(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"old.example.com"}}}, EncryptedValue: []byte(`{"token":"old"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex",
		Bundle: "img:1", RunCommand: []string{"codex"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := putHarnessBinding(ctx, st, harnessBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "TOKEN", SecretID: previous.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
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
	if !reflect.DeepEqual(updated.Routes, []model.SecretRoute{{Hosts: []string{"old.example.com"}}}) {
		t.Fatalf("routes = %+v, want the routes it had kept", updated.Routes)
	}
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil || len(bindings) != 1 || bindings[0].SecretID != previous.ID {
		t.Fatalf("bindings = %+v, %v; want the standing binding kept", bindings, err)
	}
}

// `shell` is seeded from the registry like any other harness — same inspect,
// same snapshot — and what it declares is what it gets. The `docker` group is
// why that matters: the sandbox ships the Docker CLI, which checks group
// membership, so without it Docker works as the sandbox user under a coding
// harness and not under a plain shell.
//
// It carries no run command, which is the declaration that the sandbox resolves
// the login shell, and it is born configured because it collects no credentials
// — derived from the empty secret list, not from its slug (ADR 0043).
func TestSeedBuiltInsSeedsShellLikeAnyOtherHarness(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const bundle = "discobox-harness-shell:local"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		bundle: {BundleDigest: "sha256:shell", ImageMetadata: harness.ImageMetadata{
			APIVersion:       harness.ImageAPIVersion,
			AdditionalGroups: []string{"docker"},
			Harness:          &harness.Image{ID: "shell", Name: "Shell"},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"shell": bundle})}
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
	if got.Bundle != bundle || got.BundleDigest != "sha256:shell" {
		t.Fatalf("bundle = %q digest = %q, want the inspected bundle", got.Bundle, got.BundleDigest)
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

	const bundle = "discobox-harness-codex:local"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		bundle: {BundleDigest: "sha256:codex", ImageMetadata: harness.ImageMetadata{
			Harness: &harness.Image{
				ID: "codex", Name: "Codex", RunCommand: []string{"codex"},
				Secrets: []harness.Secret{{Name: "OPENAI_API_KEY", Required: true}},
			},
		}},
	}}
	svc := &Service{store: st, inspector: inspector, harnesses: bundleReferences(map[string]string{"codex": bundle})}
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
		Bundle: "discobox-harness-shell:local",
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
	created       services.CreateSandboxBody
	rebound       []string
	upgraded      []string
	marked        []string
	egressChanged []string
	deleteErr     error
}

func (s *stubSandboxRuntime) MarkProxyPolicyChangedTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, _, harnessConfigID string) error {
	s.egressChanged = append(s.egressChanged, harnessConfigID)
	return nil
}

func (s *stubSandboxRuntime) MarkSecretHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, secretID string) error {
	s.marked = append(s.marked, secretID)
	return nil
}

func (s *stubSandboxRuntime) AcquireSandboxHTTPClientForServer(ctx context.Context, p, b string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return s.AcquireSandboxHTTPClient(ctx, p, b, scopes, use)
}

// MarkBindingHoldersDirtyTx records the targets whose holders a binding
// change reached.
func (s *stubSandboxRuntime) MarkBindingHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _ string, bindings []model.SecretBinding) error {
	for _, binding := range bindings {
		if !slices.Contains(s.rebound, binding.TargetID) {
			s.rebound = append(s.rebound, binding.TargetID)
		}
	}
	return nil
}

func (s *stubSandboxRuntime) MaterializeSandboxSecretsTx(context.Context, *store.Store, string, string) error {
	return nil
}

// harnessBinding is a harness config's use of a secret in a variable, as the
// tests write it.
type harnessBinding struct {
	ProjectID, HarnessConfigID, EnvName, SecretID string
}

// putHarnessBinding binds a secret for use in a variable on a harness config,
// as configure does.
func putHarnessBinding(ctx context.Context, st *store.Store, b harnessBinding) error {
	_, err := st.PutTargetUseBinding(ctx, &model.SecretBinding{
		ProjectID: b.ProjectID, SecretID: b.SecretID, TargetKind: model.SecretBindingTargetHarnessConfig,
		TargetID: b.HarnessConfigID, Use: true, EnvName: b.EnvName,
	}, store.BindingChange{Actor: "user-1"})
	return err
}

func (s *stubSandboxRuntime) UpgradeHarnessConfigSandboxes(_ context.Context, _, harnessConfigID string) error {
	s.upgraded = append(s.upgraded, harnessConfigID)
	return nil
}

func (s *stubSandboxRuntime) CreateSandbox(_ context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	s.created = input
	return &model.Sandbox{ID: "sandbox-1", ProjectID: projectID}, nil
}

func (s *stubSandboxRuntime) DeleteSandbox(context.Context, string, string) error { return s.deleteErr }

func (s *stubSandboxRuntime) AcquireSandboxHTTPClient(context.Context, string, string, []string, services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return nil, nil, errors.New("not used")
}

type stubDirtier struct{}

func (stubDirtier) MarkDirtyTx(context.Context, *gorm.DB, string, string) error { return nil }

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
		Bundle: "discobox-harness-claude-code:local", BundleDigest: "sha256:claude",
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

// A harness registered before bundles has nothing a configure sandbox can be
// pinned to — no bundle — so configure refuses it, naming
// what to do, before any sandbox is created.
func TestConfigureRefusesAHarnessRegisteredBeforeBundles(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "legacy", Name: "Legacy",
		ConfigCommand: []string{"configure"},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}

	runtime := &stubSandboxRuntime{}
	svc := &Service{store: st, inspector: &stubInspector{}, sandboxes: runtime, dirtier: stubDirtier{}}
	_, err := svc.ConfigureHarnessConfig(ctx, "project-1", config.ID)
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "register its bundle") {
		t.Fatalf("err = %v, want a 400 naming what to do", err)
	}
	if runtime.created.Config.Name != "" {
		t.Fatalf("created a configure sandbox %q for a harness with nothing to pin it to", runtime.created.Config.Name)
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
		Bundle: "img:1", RunCommand: []string{"claude"},
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
	if secret.MaxBindingTTL != 0 {
		t.Fatalf("binding limit = %d, want none: the harness's own binding never expires", secret.MaxBindingTTL)
	}

	// And the binding it stands on says the same thing from the other side.
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].ExpiresAt != nil {
		t.Fatalf("bindings = %+v, want the one the configure flow made, never expiring", bindings)
	}
}

// The reaper's job is to let go of a configure sandbox that is already gone, so
// a sandbox the runtime no longer knows about has to read as success. The
// runtime answers a missing sandbox with the 404 it serves the API, and that
// error still has to match the store sentinel: when it did not, the reap failed
// forever, ConfigureSandboxID was never cleared, and the reconciler logged
// "sandbox not found" every requeue for the life of the process.
func TestReconcileReapsAConfigureSandboxThatIsAlreadyGone(t *testing.T) {
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
	st := store.New(db.Write, db.Read)

	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code",
		Bundle:             "img:1",
		ConfigCommand:      []string{"configure"},
		ConfigureSandboxID: "sbx_gone",
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	// Past the TTL, so the reap runs rather than requeueing.
	if err := db.Write.WithContext(ctx).Model(&model.HarnessConfig{}).
		Where("id = ?", config.ID).
		UpdateColumn("updated_at", time.Now().Add(-2*configureTTL)).Error; err != nil {
		t.Fatalf("backdate harness config: %v", err)
	}

	runtime := &stubSandboxRuntime{
		deleteErr: apperrors.NotFound(store.ErrNotFound, "sandbox not found"),
	}
	svc := &Service{store: st, inspector: &stubInspector{}, sandboxes: runtime, dirtier: stubDirtier{}}
	if _, err := svc.Reconcile(ctx, config.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	reaped, err := st.GetHarnessConfigByID(ctx, config.ID)
	if err != nil {
		t.Fatalf("get harness config: %v", err)
	}
	if reaped.ConfigureSandboxID != "" {
		t.Fatalf("configure sandbox id = %q, want it cleared", reaped.ConfigureSandboxID)
	}
}

// Re-running a harness's sign-in is the remedy the window offers for a
// credential an upstream refused (ADR 0132 §5), so it has to retract what was
// recorded about the credential it replaced. Otherwise the user does the one
// thing they were asked to do and the band is still there.
func TestApplyConfigureOutputClearsARefusedCredential(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	previous := &model.Secret{ProjectID: "project-1", Name: "old-token", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"api.example.com"}}}, EncryptedValue: []byte(`{"token":"dead"}`)}
	if err := st.CreateSecret(ctx, previous); err != nil {
		t.Fatalf("create previous secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex",
		Bundle: "img:1", RunCommand: []string{"codex"},
		Configured: true, ConfiguredSecretIDs: []string{previous.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := putHarnessBinding(ctx, st, harnessBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "TOKEN", SecretID: previous.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := st.RecordSecretRejection(ctx, &model.SecretRejection{
		ProjectID: "project-1", SecretID: previous.ID, Host: "api.example.com",
		Reason: model.SecretRejectionReasonUnrefreshable, SandboxID: "sandbox-1",
	}); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "TOKEN", Name: "new-token", Type: "bearer", Host: "api.example.com", Value: []byte(`{"token":"fresh"}`),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rejections, err := st.ListSecretRejections(ctx, "project-1")
	if err != nil {
		t.Fatalf("list rejections: %v", err)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %v, want none: the sign-in that replaced the credential is the remedy", rejections)
	}
}

// The type a configure command declares is the one field of its output that is
// a closed vocabulary, and the only writer of Secret.Type that does not come
// from the API's validated enum. An unknown value is not cosmetic: the API
// validates the enum on the way out too, so one bad row fails to serialize and
// takes the whole project's secret listing with it in every client.
func TestApplyConfigureOutputHoldsSecretsToTheTwoTypes(t *testing.T) {
	ctx := context.Background()

	// Shipped harness images still say "bearer" — claude-code and codex-cli
	// both do for their API-key path — and an image is not upgraded in step
	// with a server, so it is renamed rather than refused.
	for _, declared := range []string{"bearer", "", "TOKEN"} {
		t.Run("normalized/"+declared, func(t *testing.T) {
			st := newTestStore(t)
			config := &model.HarnessConfig{
				ProjectID: "project-1", Slug: "codex", Name: "Codex",
				Bundle: "img:1", RunCommand: []string{"codex"},
			}
			if err := st.CreateHarnessConfig(ctx, config); err != nil {
				t.Fatalf("create config: %v", err)
			}
			svc := &Service{store: st, inspector: &stubInspector{}}
			out := &configureOutput{Secrets: []configureSecret{{
				EnvName: "OPENAI_API_KEY", Name: "key", Type: declared, Value: []byte(`{"token":"sk-real"}`),
			}}}
			if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
				t.Fatalf("apply: %v", err)
			}
			secrets, err := st.ListSecrets(ctx, "project-1")
			if err != nil {
				t.Fatalf("list secrets: %v", err)
			}
			if len(secrets) != 1 || secrets[0].Type != model.SecretTypeToken {
				t.Fatalf("stored types = %v, want one %q", secrets, model.SecretTypeToken)
			}
		})
	}

	// Anything else is refused here, where the failure can name the harness and
	// the variable, rather than at a read weeks later that cannot say where the
	// row came from.
	st := newTestStore(t)
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex",
		Bundle: "img:1", RunCommand: []string{"codex"},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	svc := &Service{store: st, inspector: &stubInspector{}}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "OPENAI_API_KEY", Name: "key", Type: "ssh", Value: []byte(`{"token":"sk-real"}`),
	}}}
	err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out)
	if err == nil {
		t.Fatal("an unknown secret type was stored, want the configure refused")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") || !strings.Contains(err.Error(), "ssh") {
		t.Fatalf("error = %v, want it to name the variable and the type", err)
	}
	if secrets, _ := st.ListSecrets(ctx, "project-1"); len(secrets) != 0 {
		t.Fatalf("stored secrets = %v, want none", secrets)
	}
}

// Configure writes the routes the image declares onto the secret it creates,
// and a later image declaring different ones re-syncs them (ADR 26-10-02-924).
func TestApplyConfigureOutputWritesDeclaredRoutesOntoTheSecret(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "copilot", Name: "GitHub Copilot",
		Bundle: "img:1", RunCommand: []string{"copilot"},
		Secrets: []model.HarnessConfigSecret{{Name: "COPILOT_GITHUB_TOKEN", Routes: []model.SecretRoute{
			{Hosts: []string{"githubcopilot.com"}},
			{Hosts: []string{"github.com"}, Blocked: true},
		}}},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	runtime := &stubSandboxRuntime{}
	svc := &Service{store: st, inspector: &stubInspector{}, sandboxes: runtime}
	out := &configureOutput{Secrets: []configureSecret{{
		EnvName: "COPILOT_GITHUB_TOKEN", Type: "bearer", Value: []byte("token-value"),
	}}}
	if err := svc.applyConfigureOutput(ctx, config, "sandbox-1", out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(config.ConfiguredSecretIDs) != 1 {
		t.Fatalf("configured secrets = %v, want one", config.ConfiguredSecretIDs)
	}
	secretID := config.ConfiguredSecretIDs[0]
	secret, err := st.GetSecret(ctx, "project-1", secretID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secret.Routes, []model.SecretRoute{{Hosts: []string{"githubcopilot.com"}}, {Hosts: []string{"github.com"}, Blocked: true}}) {
		t.Fatalf("routes = %+v, want the declared ones, the blocked one blocked", secret.Routes)
	}
	kept := secret.Routes[1]
	if got := runtime.marked; len(got) != 0 {
		t.Fatalf("marked = %v, want none: the secret was minted with its routes, before anything held it", got)
	}

	config.Secrets[0].Routes = append(config.Secrets[0].Routes, model.SecretRoute{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp"}, Blocked: true})
	if err := st.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		return svc.syncConfiguredSecretRoutes(ctx, txStore, txDB, config)
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if secret, err = st.GetSecret(ctx, "project-1", secretID); err != nil {
		t.Fatal(err)
	}
	if len(secret.Routes) != 3 || !reflect.DeepEqual(secret.Routes[1], kept) || !slices.Equal(secret.Routes[2].Paths, []string{"/mcp"}) {
		t.Fatalf("routes = %+v, want the unchanged ones kept and the new one added", secret.Routes)
	}
	if got := runtime.marked; len(got) != 1 || got[0] != secretID {
		t.Fatalf("marked = %v, want the change to mark the secret's holders for reconcile", got)
	}
}
