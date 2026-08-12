package sandboxes

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

// pinnedSandboxSeq names each fixture uniquely: sandbox names are unique within
// a project, and these tests store several sandboxes in one.
var pinnedSandboxSeq atomic.Int64

// pinnedSandbox stores a sandbox pinned to an image digest, with a harness
// config to compare against.
func pinnedSandbox(t *testing.T, st *store.Store, configID, image, digest string) *model.Sandbox {
	t.Helper()
	ensurePool(t, st)
	sb := &model.Sandbox{
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		PoolID:          "pool-1",
		Name:            fmt.Sprintf("sandbox-%d", pinnedSandboxSeq.Add(1)),
		SandboxManifest: model.SandboxManifest{HarnessMode: "run", Image: image, ImageDigest: digest},
	}
	if configID != "" {
		sb.HarnessConfigID = &configID
	}
	if err := st.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sb
}

// ensurePool satisfies the sandbox's pool foreign key, once per store.
func ensurePool(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.GetPool(ctx, "project-1", "pool-1"); err == nil {
		return
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"}
	if err := st.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
}

func imagedConfig(t *testing.T, st *store.Store, image, digest string) *model.HarnessConfig {
	t.Helper()
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"},
		Image: image, ImageDigest: digest, Configured: true,
	}
	if err := st.CreateHarnessConfig(context.Background(), config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	return config
}

// A rebuilt image under the same tag is the case tag comparison misses, and the
// one dev workflows hit constantly.
func TestUpgradeTargetDetectsRebuildUnderTheSameTag(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")
	sb := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")

	target, err := svc.upgradeTarget(ctx, sb)
	if err != nil {
		t.Fatalf("upgradeTarget: %v", err)
	}
	if !target.Available || target.Digest != "sha256:new" {
		t.Fatalf("target = %+v, want available with the new digest", target)
	}
}

func TestUpgradeTargetIsUnavailableWhenDigestsMatch(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:same")
	sb := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:same")

	target, err := svc.upgradeTarget(ctx, sb)
	if err != nil {
		t.Fatalf("upgradeTarget: %v", err)
	}
	if target.Available {
		t.Fatalf("target = %+v, want unavailable", target)
	}
}

// An unpinned sandbox under an image-bearing harness config — created before
// pinning existed, or while the config's digest was unknown — is the strongest
// candidate for adopting the config's current image: its tag may no longer
// exist anywhere, and upgrade is its only way out.
func TestUpgradeTargetOffersConfigImageToUnpinnedSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")

	unpinned := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:stale", "")
	target, err := svc.upgradeTarget(ctx, unpinned)
	if err != nil {
		t.Fatalf("upgradeTarget: %v", err)
	}
	if !target.Available || target.Image != "discobox-harness-codex:local" || target.Digest != "sha256:new" {
		t.Fatalf("unpinned target = %+v, want available with the config's image and digest", target)
	}
}

// A config-mode sandbox runs the configure command against a deliberately fixed
// image, and a sandbox with no harness config has no image to move to.
func TestUpgradeTargetIgnoresConfigModeSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")

	configMode := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")
	configMode.HarnessMode = "config"
	if target, err := svc.upgradeTarget(ctx, configMode); err != nil || target.Available {
		t.Fatalf("config-mode target = %+v, %v; want unavailable", target, err)
	}
}

// shellConfig seeds the reserved built-in an unconverged sandbox upgrades to.
func shellConfig(t *testing.T, st *store.Store, image, digest string) *model.HarnessConfig {
	t.Helper()
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: harnessdefs.ShellSlug, Name: harnessdefs.ShellName,
		BuiltIn: true, Configured: true, Image: image, ImageDigest: digest,
	}
	if err := st.CreateHarnessConfig(context.Background(), config); err != nil {
		t.Fatalf("create shell config: %v", err)
	}
	return config
}

// TestUpgradeTargetForHarnesslessSandboxIsTheFallback: a sandbox created before
// every sandbox carried a harness config converges by upgrade, and what it
// upgrades to is the reserved `shell` built-in (ADR 0025 §4).
func TestUpgradeTargetForHarnesslessSandboxIsTheFallback(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	shell := shellConfig(t, st, "discobox-sandbox-agent:dev-new", "sha256:default-new")

	harnessless := pinnedSandbox(t, st, "", "discobox-sandbox-agent:dev-old", "sha256:default-old")
	target, err := svc.upgradeTarget(ctx, harnessless)
	if err != nil {
		t.Fatalf("upgrade target: %v", err)
	}
	if !target.Available {
		t.Fatalf("target = %+v, want an available upgrade to the fallback", target)
	}
	if target.Image != shell.Image || target.Digest != shell.ImageDigest {
		t.Fatalf("target = %+v, want the shell config's image", target)
	}

	// Even on the fallback's own digest: what the upgrade changes is adopting
	// the config, not the image.
	current := pinnedSandbox(t, st, "", shell.Image, shell.ImageDigest)
	if target, err := svc.upgradeTarget(ctx, current); err != nil || !target.Available {
		t.Fatalf("target = %+v, %v; want available while still unconverged", target, err)
	}
}

// TestUpgradeSandboxAdoptsTheFallbackConfig is the convergence itself: the
// upgrade writes the harness config the sandbox never had. It needs the
// reconcile engine, since the adoption rides recordSandboxIntent like any other
// spec change.
func TestUpgradeSandboxAdoptsTheFallbackConfig(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	shell := shellConfig(t, st, "discobox-sandbox-agent:dev-new", "sha256:default-new")
	harnessless := pinnedSandbox(t, st, "", "discobox-sandbox-agent:dev-old", "sha256:default-old")

	upgraded, err := svc.UpgradeSandbox(ctx, "project-1", harnessless.ID, services.UpgradeSandboxBody{})
	if err != nil {
		t.Fatalf("upgrade sandbox: %v", err)
	}
	if upgraded.HarnessConfigID == nil || *upgraded.HarnessConfigID != shell.ID {
		t.Fatalf("harness config = %v, want the adopted shell config %s", upgraded.HarnessConfigID, shell.ID)
	}
	if upgraded.Image != shell.Image || upgraded.ImageDigest != shell.ImageDigest {
		t.Fatalf("image = %s@%s, want the shell config's", upgraded.Image, upgraded.ImageDigest)
	}
	// Converged: nothing further to do.
	if target, err := svc.upgradeTarget(ctx, upgraded); err != nil || target.Available {
		t.Fatalf("target = %+v, %v; want unavailable once converged", target, err)
	}
}

// TestUpgradeTargetWithoutASeededFallback: seeding may not have created the
// `shell` config, and then an unconverged sandbox has nowhere to go yet.
func TestUpgradeTargetWithoutASeededFallback(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)

	harnessless := pinnedSandbox(t, st, "", "discobox-sandbox-agent:local", "sha256:old")
	if target, err := svc.upgradeTarget(ctx, harnessless); err != nil || target.Available {
		t.Fatalf("target = %+v, %v; want unavailable with no fallback seeded", target, err)
	}
}

// pinCapturingProvider records the image a create was actually asked to build
// from, which is the only place the effective pin is observable.
type pinCapturingProvider struct {
	recordingProvider
	created []ImageRef
}

func (p *pinCapturingProvider) Create(_ context.Context, _ sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	p.created = append(p.created, opts.Image)
	return &sandbox.Sandbox{ID: "runtime-1"}, state, nil
}

// A sandbox runs the image it is pinned to until somebody upgrades it. No
// reconcile advances the pin, whatever state the sandbox is in — including the
// non-live states that used to re-pin themselves on the way up (ADR 0021 §2).
func TestReconcileNeverRepinsToTheConfigImage(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")

	for _, state := range []string{model.SandboxStateReady, model.SandboxStateFailed, model.SandboxStatePending} {
		sb := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:gone", "sha256:old")
		sb.State = state
		sb.DesiredState = model.DesiredStatePresent
		sb.IncrementGeneration()
		if err := st.UpdateSandbox(ctx, sb); err != nil {
			t.Fatalf("state %q: update sandbox: %v", state, err)
		}
		provider := &pinCapturingProvider{}
		reconciler := NewSandboxReconciler(st, WithSandboxProvider(provider))

		if _, err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
			t.Fatalf("state %q: reconcile: %v", state, err)
		}
		if len(provider.created) != 1 {
			t.Fatalf("state %q: creates = %d, want 1", state, len(provider.created))
		}
		if got := provider.created[0]; got.Digest != "sha256:old" || got.Name != "discobox-harness-codex:gone" {
			t.Fatalf("state %q: created from %+v, want the sandbox's own pin", state, got)
		}
		stored, err := st.GetSandbox(ctx, sb.ProjectID, sb.ID)
		if err != nil {
			t.Fatalf("state %q: get sandbox: %v", state, err)
		}
		if stored.ImageDigest != "sha256:old" || stored.Image != "discobox-harness-codex:gone" {
			t.Fatalf("state %q: pin moved to %q/%q without an upgrade", state, stored.Image, stored.ImageDigest)
		}
		// The upgrade is still offered — it is reported, not applied.
		target, err := svc.upgradeTarget(ctx, stored)
		if err != nil {
			t.Fatalf("state %q: upgradeTarget: %v", state, err)
		}
		if !target.Available || target.Digest != "sha256:new" {
			t.Fatalf("state %q: target = %+v, want the config's image still on offer", state, target)
		}
	}
}

// Upgrading recreates the container, which costs anything written to the
// container filesystem. Doing that for no image change is pure loss, so it is
// refused rather than performed.
func TestUpgradeSandboxRefusedWhenAlreadyCurrent(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:same")
	sb := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:same")

	if _, err := svc.UpgradeSandbox(ctx, "project-1", sb.ID, services.UpgradeSandboxBody{}); err == nil {
		t.Fatal("upgrade succeeded; want refusal when the sandbox is already current")
	}
}

// newUpgradeEngineFixture is newBindingFixture plus the reconcile engine an
// intent-recording path needs.
func newUpgradeEngineFixture(t *testing.T) (*Service, *store.Store) {
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
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return NewService(st, nil, "user-1", engine), st
}
