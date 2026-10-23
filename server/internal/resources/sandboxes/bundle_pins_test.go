package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

const (
	platformRef   = "ghcr.io/discobox-ai/discobox-platform:v1"
	platformOld   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	platformNew   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	codexBundle   = "ghcr.io/discobox-ai/discobox-harness-codex:v1"
	codexBundleV1 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	codexBundleV2 = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	shellBundle   = "ghcr.io/discobox-ai/discobox-harness-shell:v1"
	shellBundleV1 = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	shellBundleV2 = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	// legacyImage is the runnable harness image a sandbox from before
	// bundles was pinned to, which no release builds any more.
	legacyImage       = "discobox-harness-codex:old"
	legacyImageDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
)

// fakePlatform is the release's platform as the asset service resolves it.
type fakePlatform struct {
	pin services.BundlePin
	err error
}

func (f *fakePlatform) CurrentPlatform(context.Context) (services.BundlePin, error) {
	return f.pin, f.err
}
func (f *fakePlatform) LatestPlatform() services.BundlePin { return f.pin }

// A create pins the harness config's bundle and the platform
// the release resolves to (ADR 26-10-16-423 §7), and the pins reach the pool
// as what the box may fetch, each by kind. A platform the server names and
// cannot resolve fails the create rather than leave the box pinned to none.
func TestCreatePinsTheBundles(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	platform := &fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformOld}}
	svc.SetPlatformBundles(platform)
	create := func(name string) (*model.Sandbox, error) {
		return svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config:      serverapi.SandboxCreateConfig{Name: name},
		})
	}
	created, err := create("pinned")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Bundle != codexBundle || sb.BundleDigest != codexBundleV1 || sb.PlatformBundle != platformRef || sb.PlatformBundleDigest != platformOld {
		t.Fatalf("pins = %q@%q, %q@%q", sb.Bundle, sb.BundleDigest, sb.PlatformBundle, sb.PlatformBundleDigest)
	}
	if sb.Image != "" || sb.ImageDigest != "" {
		t.Fatalf("image = %q@%q, want none: no harness is an image", sb.Image, sb.ImageDigest)
	}
	want := []BundlePin{{Kind: BundlePlatform, Name: "discobox-platform", Digest: platformOld}, {Kind: BundleHarness, Name: "discobox-harness-codex", Digest: codexBundleV1}}
	if got := sandboxBundlePins(sb); !slices.Equal(got, want) {
		t.Fatalf("pins sent to the pool = %+v, want %+v", got, want)
	}
	read, err := services.SandboxToAPI(sb, services.SandboxUpgradeBasis{})
	if err != nil {
		t.Fatal(err)
	}
	if read.Config.BundleDigest.Or("") != codexBundleV1 || read.Config.PlatformBundleDigest.Or("") != platformOld {
		t.Fatalf("pins read back = %q, %q", read.Config.BundleDigest.Or(""), read.Config.PlatformBundleDigest.Or(""))
	}

	platform.err = errors.New("registry unreachable")
	_, err = create("unpinned")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502", err)
	}

	// A server that names no platform has nothing a box on a bundle could
	// apply its harness onto.
	platform.pin, platform.err = services.BundlePin{}, nil
	_, err = create("no-platform")
	if !errors.As(err, &status) || status.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503", err)
	}
}

// A create refuses a harness config it cannot pin a box to: one registered
// before bundles, which names only the runnable image it was registered with
// (409, the config needs its bundle registered), and one whose bundle has no
// content digest, a development bundle no pool has built yet (503, it will).
func TestCreateRefusesAHarnessItCannotPin(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	for _, tc := range []struct {
		name   string
		mutate func(*model.HarnessConfig)
		status int
		says   string
	}{
		{
			name: "registered before bundles",
			// The upgrade retired the runnable image it named
			// (database.retireHarnessImages), leaving neither.
			mutate: func(c *model.HarnessConfig) { c.Bundle, c.BundleDigest = "", "" },
			status: http.StatusConflict, says: "registered before bundles",
		},
		{
			name: "an unbuilt development bundle",
			mutate: func(c *model.HarnessConfig) {
				c.Bundle, c.BundleDigest = "discobox-harness-codex:dev-0123", "discobox-harness-codex:dev-0123"
			},
			status: http.StatusServiceUnavailable, says: "has not been built yet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mutate(config)
			if err := st.UpdateHarnessConfig(ctx, config); err != nil {
				t.Fatal(err)
			}
			_, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
				HarnessName: serverapi.NewOptString("codex"),
				Config:      serverapi.SandboxCreateConfig{Name: "refused"},
			})
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != tc.status || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("err = %v, want a %d saying %q", err, tc.status, tc.says)
			}
			if listed, err := st.ListSandboxes(ctx, "project-1", "", nil); err != nil || len(listed) != 0 {
				t.Fatalf("listed %d sandboxes (%v), want the refused create to leave none", len(listed), err)
			}
		})
	}
}

// A configure box runs the harness's configure command, which the harness
// bundle delivers: it is pinned to its harness config's bundle and the
// platform exactly as a run is, not to the platform alone.
func TestConfigModeCreatePinsTheHarnessBundle(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	config.Configured = false
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	svc.SetPlatformBundles(&fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformOld}})
	body := services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: "configure"}}
	body.Config.SetHarnessConfigId(serverapi.NewOptString(config.ID))
	body.Config.SetHarnessMode(serverapi.NewOptSandboxCreateConfigHarnessMode(serverapi.SandboxCreateConfigHarnessModeConfig))
	created, err := svc.CreateSandbox(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("create configure box: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Image != "" || sb.Bundle != codexBundle || sb.BundleDigest != codexBundleV1 {
		t.Fatalf("configure box pins = %q, %q@%q, want the harness config's bundle alone", sb.Image, sb.Bundle, sb.BundleDigest)
	}
	want := []BundlePin{{Kind: BundlePlatform, Name: "discobox-platform", Digest: platformOld}, {Kind: BundleHarness, Name: "discobox-harness-codex", Digest: codexBundleV1}}
	if got := sandboxBundlePins(sb); !slices.Equal(got, want) {
		t.Fatalf("pins sent to the pool = %+v, want %+v", got, want)
	}
}

// A pin with no content digest, a build-mode development image's, has nothing
// to fetch, and a box is delivered nothing it cannot fetch: no pin is sent,
// rather than a platform with no harness.
func TestOnlyContentDigestsAreSentToThePool(t *testing.T) {
	sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Bundle: codexBundle, BundleDigest: "discobox-harness-codex:local", PlatformBundle: platformRef, PlatformBundleDigest: platformOld}}
	if got := sandboxBundlePins(sb); got != nil {
		t.Fatalf("pins = %+v, want none", got)
	}
}

// A stopped sandbox follows the release's platform when it moves; a running
// one keeps its pin and reports the upgrade (ADR 26-10-16-423 §7, ADR 0082).
func TestThePlatformMovingCarriesStoppedSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := bundledConfig(t, st, codexBundle, codexBundleV1)
	onOld := func(sb *model.Sandbox) {
		sb.PlatformBundle, sb.PlatformBundleDigest = platformRef, platformOld
	}
	stopped := eligibleSandbox(t, st, config.ID, codexBundle, codexBundleV1, onOld)
	running := eligibleSandbox(t, st, config.ID, codexBundle, codexBundleV1, onOld, func(sb *model.Sandbox) {
		sb.RuntimeState = model.SandboxRuntimeStateRunning
	})
	svc.SetPlatformBundles(&fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformNew}})

	if err := svc.UpgradePlatformSandboxes(ctx); err != nil {
		t.Fatalf("upgrade platform sandboxes: %v", err)
	}
	moved, err := st.GetSandbox(ctx, "project-1", stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.PlatformBundleDigest != platformNew || moved.Generation <= stopped.Generation {
		t.Fatalf("stopped sandbox platform = %q at generation %d, want %q past %d", moved.PlatformBundleDigest, moved.Generation, platformNew, stopped.Generation)
	}
	kept, err := st.GetSandbox(ctx, "project-1", running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.PlatformBundleDigest != platformOld {
		t.Fatalf("running sandbox platform = %q, want it left on %q", kept.PlatformBundleDigest, platformOld)
	}
	kept.HarnessConfig = config
	basis, err := svc.UpgradeBasis(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if upgrade := services.SandboxUpgrade(kept, basis); upgrade["available"] != true || upgrade["reason"] != "platformBundleDigestChanged" {
		t.Fatalf("running sandbox upgrade = %v, want the platform available", upgrade)
	}
}

// A sandbox from before bundles, pinned to the runnable harness image it ran,
// is carried off it by its harness config's bundle moving: the re-pin writes
// the bundle and leaves the image behind, which nothing builds any more.
func TestABundleMovingCarriesAStoppedSandboxOffItsImage(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := bundledConfig(t, st, codexBundle, codexBundleV2)
	sb := eligibleSandbox(t, st, config.ID, "", "", func(sb *model.Sandbox) {
		sb.Image, sb.ImageDigest = legacyImage, legacyImageDigest
	})
	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Bundle != codexBundle || stored.BundleDigest != codexBundleV2 || stored.Image != "" || stored.ImageDigest != "" {
		t.Fatalf("pins = %q@%q, image %q@%q, want the config's bundle and no image",
			stored.Bundle, stored.BundleDigest, stored.Image, stored.ImageDigest)
	}
}

// Every settled sandbox no container runs that is pinned to no platform — one
// from before bundles — is pinned to its harness config's bundle and the
// release's platform, under a manual upgrade policy too: the image it ran is
// gone either way (ADR 26-10-16-423 §10). A stopped one is recorded intent, so
// its pool rebuilds it; an archived one is pinned in place, for the unarchive
// that builds one. A running one, one whose config has no bundle to pin, and
// one already delivered its platform are left alone.
func TestUndeliveredSandboxesArePinned(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	project, err := st.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	project.SandboxUpgradePolicy = model.SandboxUpgradePolicyManual
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	config := bundledConfig(t, st, codexBundle, codexBundleV1)
	legacy := func(sb *model.Sandbox) { sb.Image, sb.ImageDigest = legacyImage, legacyImageDigest }
	stopped := eligibleSandbox(t, st, config.ID, "", "", legacy)
	running := eligibleSandbox(t, st, config.ID, "", "", legacy, func(sb *model.Sandbox) {
		sb.RuntimeState = model.SandboxRuntimeStateRunning
	})
	archived := eligibleSandbox(t, st, config.ID, "", "", legacy, func(sb *model.Sandbox) {
		sb.DesiredState, sb.State = model.DesiredStateArchived, model.SandboxStateArchived
	})
	delivered := eligibleSandbox(t, st, config.ID, codexBundle, codexBundleV1, func(sb *model.Sandbox) {
		sb.PlatformBundle, sb.PlatformBundleDigest = platformRef, platformOld
	})
	// Registered before bundles: the upgrade retired the runnable image it
	// named (database.retireHarnessImages), and no bundle is registered yet.
	unregistered := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "legacy", Name: "Legacy", RunCommand: []string{"legacy"}, Configured: true,
	}
	if err := st.CreateHarnessConfig(ctx, unregistered); err != nil {
		t.Fatal(err)
	}
	noBundle := eligibleSandbox(t, st, unregistered.ID, "", "", legacy)
	svc.SetPlatformBundles(&fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformNew}})

	if err := svc.PinUndeliveredSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		sb             *model.Sandbox
		bundle         string
		platform       string
		image          string
		wantGeneration bool
	}{
		{"stopped", stopped, codexBundleV1, platformNew, "", true},
		{"archived", archived, codexBundleV1, platformNew, "", false},
		{"running", running, "", "", legacyImageDigest, false},
		{"delivered", delivered, codexBundleV1, platformOld, "", false},
		{"on a config with no bundle", noBundle, "", "", legacyImageDigest, false},
	} {
		stored, err := st.GetSandbox(ctx, "project-1", tc.sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.BundleDigest != tc.bundle || stored.PlatformBundleDigest != tc.platform || stored.ImageDigest != tc.image ||
			(stored.Generation > tc.sb.Generation) != tc.wantGeneration {
			t.Fatalf("%s: bundle %q platform %q image %q at generation %d (was %d), want %q, %q, %q and a bump %v",
				tc.name, stored.BundleDigest, stored.PlatformBundleDigest, stored.ImageDigest, stored.Generation, tc.sb.Generation,
				tc.bundle, tc.platform, tc.image, tc.wantGeneration)
		}
	}

	// Nothing is pinned while the platform cannot be resolved: a box on a
	// bundle with no platform has nothing to apply it onto.
	again := eligibleSandbox(t, st, config.ID, "", "", legacy)
	svc.SetPlatformBundles(&fakePlatform{err: errors.New("registry unreachable")})
	if err := svc.PinUndeliveredSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	if stored, err := st.GetSandbox(ctx, "project-1", again.ID); err != nil || stored.BundleDigest != "" || stored.Generation != again.Generation {
		t.Fatalf("pinned %+v (%v) with no platform resolved, want it left for a later pass", stored, err)
	}
}

// A sandbox with no harness config that moves onto the platform alone, its
// fallback offering no bundle, keeps its bundle and adopts no config: the old
// bundle beside the fallback's command is a harness that does not start.
func TestAPlatformOnlyRepinAdoptsNoConfig(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	shellConfig(t, st, shellBundle, "")
	sb := eligibleSandbox(t, st, "", shellBundle, shellBundleV1, func(sb *model.Sandbox) {
		sb.PlatformBundle, sb.PlatformBundleDigest = platformRef, platformOld
	})
	svc.SetPlatformBundles(&fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformNew}})

	if err := svc.UpgradePlatformSandboxes(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlatformBundleDigest != platformNew || stored.BundleDigest != shellBundleV1 || stored.HarnessConfigID != nil {
		t.Fatalf("platform %q bundle %q config %v, want the platform moved, the bundle kept and no config adopted",
			stored.PlatformBundleDigest, stored.BundleDigest, stored.HarnessConfigID)
	}
}

// A sandbox delivered its platform is not rebuilt by a moved image or
// platform pin: its box applies its platform at every start, and runs the
// sandbox base image. A moved harness bundle pin still rebuilds it, since the
// harness's manifest is what its container is built with. A sandbox with no
// platform pin keeps the fingerprint it had (ADR 26-10-16-423 §7).
func TestADeliveredSandboxIsNotRebuiltByAMovedPlatform(t *testing.T) {
	sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{
		Image: "discobox-harness-codex-image:v1", ImageDigest: "sha256:image-v1",
		Bundle: codexBundle, BundleDigest: codexBundleV1,
		PlatformBundle: platformRef, PlatformBundleDigest: platformOld,
	}}
	before := specFingerprint(sb, sandboxBundlePins(sb))
	if before == sb.Fingerprint() {
		t.Fatal("a delivered sandbox has its manifest's fingerprint")
	}

	moved := *sb
	moved.Image, moved.ImageDigest = "discobox-harness-codex-image:v2", "sha256:image-v2"
	moved.PlatformBundleDigest = "sha256:" + strings.Repeat("5", 64)
	if got := specFingerprint(&moved, sandboxBundlePins(&moved)); got != before {
		t.Fatal("a moved platform or image pin changed a delivered sandbox's fingerprint")
	}
	moved.BundleDigest = "sha256:" + strings.Repeat("4", 64)
	if got := specFingerprint(&moved, sandboxBundlePins(&moved)); got == before {
		t.Fatal("a moved harness bundle left a delivered sandbox's fingerprint as it was")
	}

	legacy := &model.Sandbox{SandboxManifest: model.SandboxManifest{Image: "discobox-harness-codex-image:v1", ImageDigest: "sha256:image-v1"}}
	if got := specFingerprint(legacy, sandboxBundlePins(legacy)); got != legacy.Fingerprint() {
		t.Fatal("a sandbox delivered no platform lost the fingerprint it had")
	}
}
