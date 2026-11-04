package harnessconfigs

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// Built-ins track their bundle: a dev rebuild changes the tag, and seeding
// clobbers it and re-snapshots the label without disturbing the saved overlay.
func TestSeedBuiltInsClobbersBundleAndKeepsTheSavedOverlay(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const newBundle = "discobox-harness-codex:dev-new"
	existing := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:dev-old", BundleDigest: "sha256:old",
		RunCommand:      []string{"old"},
		ConfiguredFiles: []model.HarnessConfigFile{{Path: "auth.json", Content: "secret"}},
	}
	if err := st.CreateHarnessConfig(ctx, existing); err != nil {
		t.Fatalf("create config: %v", err)
	}

	inspector := &stubInspector{byImage: map[string]imageMetadata{
		newBundle: {BundleDigest: "sha256:new", ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex", "--new"},
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
	// Rebuilding must not drop what discoboxes saved to the harness.
	if len(got.ConfiguredFiles) != 1 {
		t.Fatalf("saved files = %v, want preserved", got.ConfiguredFiles)
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
// deleting one would silently come back. Reset is what returns it to its defaults.
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

// `shell` is seeded from the registry like any other harness — same inspect,
// same snapshot — and what it declares is what it gets. The `docker` group is
// why that matters: the sandbox ships the Docker CLI, which checks group
// membership, so without it Docker works as the sandbox user under a coding
// harness and not under a plain shell.
//
// It carries no run command, which is the declaration that the sandbox resolves
// the login shell (ADR 0043).
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
	if !got.BuiltIn {
		t.Fatal("builtIn = false, want a built-in")
	}
}

// Reset removes exactly what saved harness changes created — their files and
// the secrets they minted, with those secrets' bindings — and leaves the
// bundle's baseline and a secret a person bound by hand (ADR 26-11-02-152 §3).
func TestResetRemovesOnlyWhatSavesCreated(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	saved := &model.Secret{ProjectID: "project-1", Name: "from-save", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"save.example.com"}}}, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, saved); err != nil {
		t.Fatalf("create saved secret: %v", err)
	}
	byHand := &model.Secret{ProjectID: "project-1", Name: "user-owned", Type: model.SecretTypeToken, Routes: []model.SecretRoute{{Hosts: []string{"user.example.com"}}}, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := st.CreateSecret(ctx, byHand); err != nil {
		t.Fatalf("create hand-bound secret: %v", err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Bundle: "discobox-harness-codex:local", RunCommand: []string{"codex"},
		Files:               []model.HarnessConfigFile{{Path: "baseline.json", Content: "{}"}},
		ConfiguredFiles:     []model.HarnessConfigFile{{Path: ".codex/config.toml", Content: "model = \"saved\"\n"}},
		ConfiguredSecretIDs: []string{saved.ID},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create config: %v", err)
	}
	for env, secretID := range map[string]string{"OPENAI_API_KEY": saved.ID, "EXTRA_TOKEN": byHand.ID} {
		if err := putHarnessBinding(ctx, st, harnessBinding{ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: env, SecretID: secretID}); err != nil {
			t.Fatalf("bind %s: %v", env, err)
		}
	}

	runtime := &stubSandboxRuntime{}
	svc := &Service{store: st, inspector: &stubInspector{}, sandboxes: runtime}
	got, err := svc.ResetHarnessConfig(ctx, "project-1", config.ID, false)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(got.ConfiguredFiles) != 0 || len(got.ConfiguredSecretIDs) != 0 {
		t.Fatalf("saved overlay not cleared: files=%v secrets=%v", got.ConfiguredFiles, got.ConfiguredSecretIDs)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "baseline.json" || !slices.Equal(got.RunCommand, []string{"codex"}) {
		t.Fatalf("baseline = files %v, run %v; want it preserved", got.Files, got.RunCommand)
	}
	if _, err := st.GetSecret(ctx, "project-1", saved.ID); err == nil {
		t.Fatal("the secret a save created still exists, want it deleted")
	}
	if _, err := st.GetSecret(ctx, "project-1", byHand.ID); err != nil {
		t.Fatalf("the hand-bound secret was removed: %v", err)
	}
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].SecretID != byHand.ID {
		t.Fatalf("bindings = %+v, want only the hand-bound one", bindings)
	}
	if !slices.Contains(runtime.marked, saved.ID) {
		t.Fatalf("marked = %v, want the holders of the deleted secret marked", runtime.marked)
	}

	// A harness with nothing saved resets to what it already is.
	again, err := svc.ResetHarnessConfig(ctx, "project-1", config.ID, false)
	if err != nil {
		t.Fatalf("reset again: %v", err)
	}
	if len(again.ConfiguredFiles) != 0 {
		t.Fatalf("second reset left files %v", again.ConfiguredFiles)
	}
}

// The refusal to delete a built-in points at reset only where reset would do
// something: sending someone to a command that changes nothing is worse than
// saying nothing.
func TestBuiltInDeleteHintOnlySuggestsWhatWouldWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config *model.HarnessConfig
		want   string
	}{
		{
			name:   "saved changes, so it can be reset",
			config: &model.HarnessConfig{Slug: "codex", ConfiguredFiles: []model.HarnessConfigFile{{Path: "a", Content: "b"}}},
			want:   "; run `discobox admin harness reset codex` to return it to its bundle's defaults",
		},
		{
			name:   "a saved credential alone",
			config: &model.HarnessConfig{Slug: "codex", ConfiguredSecretIDs: []string{"sec_1"}},
			want:   "; run `discobox admin harness reset codex` to return it to its bundle's defaults",
		},
		{
			name:   "nothing saved, so it is already at its defaults",
			config: &model.HarnessConfig{Slug: "shell"},
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
