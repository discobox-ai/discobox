package sandboxes

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

// pinnedSandbox stores a sandbox pinned to an image digest, with a harness
// config to compare against.
func pinnedSandbox(t *testing.T, st *store.Store, configID, image, digest string) *model.Sandbox {
	t.Helper()
	ensurePool(t, st)
	sb := &model.Sandbox{
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		PoolID:          "pool-1",
		Name:            "sandbox",
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
func TestUpgradeTargetIgnoresConfigModeAndHarnesslessSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:new")

	configMode := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:old")
	configMode.HarnessMode = "config"
	if target, err := svc.upgradeTarget(ctx, configMode); err != nil || target.Available {
		t.Fatalf("config-mode target = %+v, %v; want unavailable", target, err)
	}

	harnessless := pinnedSandbox(t, st, "", "discobox-harness-codex:local", "sha256:old")
	if target, err := svc.upgradeTarget(ctx, harnessless); err != nil || target.Available {
		t.Fatalf("harnessless target = %+v, %v; want unavailable", target, err)
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

	for _, state := range []string{model.SandboxStateStopped, model.SandboxStateFailed, model.SandboxStatePending} {
		sb := pinnedSandbox(t, st, config.ID, "discobox-harness-codex:gone", "sha256:old")
		sb.State = state
		sb.DesiredState = model.DesiredStatePresent
		sb.IncrementGeneration()
		if err := st.UpdateSandbox(ctx, sb); err != nil {
			t.Fatalf("state %q: update sandbox: %v", state, err)
		}
		provider := &pinCapturingProvider{}
		reconciler := NewSandboxReconciler(st, WithSandboxProvider(provider))

		if err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
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
