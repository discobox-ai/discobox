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

// A create pins the harness config's bundle beside its image and the platform
// the release resolves to (ADR 26-10-16-423 §7), and the pins reach the pool
// as what the box may fetch, each by kind. A platform the server names and
// cannot resolve fails the create rather than leave the box pinned to none.
func TestCreatePinsTheBundles(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	config.Bundle, config.BundleDigest = codexBundle, codexBundleV1
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
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
}

// A configure box runs the harness's configure command, which the harness
// bundle delivers: running its harness config's image, it is pinned to that
// image and bundle as a run is, not to the platform alone. One naming another
// image has no bundle that is that image's twin, and is pinned to none.
func TestConfigModeCreatePinsTheHarnessBundle(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	config.Configured = false
	config.Bundle, config.BundleDigest = codexBundle, codexBundleV1
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	svc.SetPlatformBundles(&fakePlatform{pin: services.BundlePin{Reference: platformRef, Digest: platformOld}})
	create := func(name, image string) *model.Sandbox {
		t.Helper()
		body := services.CreateSandboxBody{Config: serverapi.SandboxCreateConfig{Name: name}}
		body.Config.SetHarnessConfigId(serverapi.NewOptString(config.ID))
		body.Config.SetHarnessMode(serverapi.NewOptSandboxCreateConfigHarnessMode(serverapi.SandboxCreateConfigHarnessModeConfig))
		body.Config.SetImage(serverapi.NewOptString(image))
		created, err := svc.CreateSandbox(ctx, "project-1", body)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		sb, err := st.GetSandbox(ctx, "project-1", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}

	sb := create("configure", config.Image)
	if sb.Image != config.Image || sb.ImageDigest != config.ImageDigest || sb.Bundle != codexBundle || sb.BundleDigest != codexBundleV1 {
		t.Fatalf("configure box pins = %q@%q, %q@%q, want the harness config's", sb.Image, sb.ImageDigest, sb.Bundle, sb.BundleDigest)
	}
	want := []BundlePin{{Kind: BundlePlatform, Name: "discobox-platform", Digest: platformOld}, {Kind: BundleHarness, Name: "discobox-harness-codex", Digest: codexBundleV1}}
	if got := sandboxBundlePins(sb); !slices.Equal(got, want) {
		t.Fatalf("pins sent to the pool = %+v, want %+v", got, want)
	}

	other := create("other-image", "ghcr.io/example/other:v1")
	if other.Image != "ghcr.io/example/other:v1" || other.ImageDigest != "" || other.Bundle != "" || other.BundleDigest != "" {
		t.Fatalf("other-image box pins = %q@%q, %q@%q, want the request's image and no harness pins", other.Image, other.ImageDigest, other.Bundle, other.BundleDigest)
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
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:image")
	onOld := func(sb *model.Sandbox) {
		sb.PlatformBundle, sb.PlatformBundleDigest = platformRef, platformOld
	}
	stopped := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image", onOld)
	running := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image", onOld, func(sb *model.Sandbox) {
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

// A harness config's bundle moving is a re-pin of its own, though its image did
// not move: a stopped sandbox takes it.
func TestABundleMovingCarriesStoppedSandboxes(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:image")
	config.Bundle, config.BundleDigest = codexBundle, codexBundleV2
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	sb := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image", func(sb *model.Sandbox) {
		sb.Bundle, sb.BundleDigest = codexBundle, codexBundleV1
	})
	if err := svc.UpgradeHarnessConfigSandboxes(ctx, "project-1", config.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BundleDigest != codexBundleV2 || stored.ImageDigest != "sha256:image" {
		t.Fatalf("pins = %q / %q, want the config's bundle beside the same image", stored.ImageDigest, stored.BundleDigest)
	}
}

// A stopped sandbox from before bundles is pinned to its harness config's
// bundle when it runs the image that bundle twins, as recorded intent, so its
// pool is sent the create that records the pin. A running one, one on another
// image, and one already pinned are left alone.
func TestStoppedSandboxesFromBeforeBundlesArePinned(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	config := imagedConfig(t, st, "discobox-harness-codex:local", "sha256:image")
	config.Bundle, config.BundleDigest = codexBundle, codexBundleV1
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	stopped := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image")
	running := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image", func(sb *model.Sandbox) {
		sb.RuntimeState = model.SandboxRuntimeStateRunning
	})
	behind := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:older")
	pinned := eligibleSandbox(t, st, config.ID, "discobox-harness-codex:local", "sha256:image", func(sb *model.Sandbox) {
		sb.Bundle, sb.BundleDigest = codexBundle, codexBundleV2
	})

	if err := svc.PinStoppedSandboxBundles(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sb             *model.Sandbox
		digest         string
		wantGeneration bool
	}{
		{stopped, codexBundleV1, true},
		{running, "", false},
		{behind, "", false},
		{pinned, codexBundleV2, false},
	} {
		stored, err := st.GetSandbox(ctx, "project-1", tc.sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.BundleDigest != tc.digest || (stored.Generation > tc.sb.Generation) != tc.wantGeneration {
			t.Fatalf("%s: bundle %q at generation %d (was %d), want %q and a bump %v",
				tc.sb.Name, stored.BundleDigest, stored.Generation, tc.sb.Generation, tc.digest, tc.wantGeneration)
		}
	}
}

// A sandbox with no harness config that moves onto the platform alone, its
// fallback offering no image, keeps its image and adopts no config: the old
// image beside the fallback's command is a harness that does not start.
func TestAPlatformOnlyRepinAdoptsNoConfig(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	shellConfig(t, st, "discobox-sandbox-agent:dev", "")
	sb := eligibleSandbox(t, st, "", "discobox-sandbox-agent:old", "sha256:old", func(sb *model.Sandbox) {
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
	if stored.PlatformBundleDigest != platformNew || stored.ImageDigest != "sha256:old" || stored.HarnessConfigID != nil {
		t.Fatalf("platform %q image %q config %v, want the platform moved, the image kept and no config adopted",
			stored.PlatformBundleDigest, stored.ImageDigest, stored.HarnessConfigID)
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
