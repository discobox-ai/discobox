package services

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// contentDigest is a content digest standing in for name's build: a bundle
// pins a sandbox only once it has one (HarnessConfigPins).
func contentDigest(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sandboxWithHarness(configID string, bundle, digest string) *model.Sandbox {
	sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Bundle: bundle, BundleDigest: digest, HarnessMode: "run"}}
	if configID != "" {
		sb.HarnessConfigID = &configID
	}
	return sb
}

const shellBundle = "ghcr.io/discobox-ai/discobox-harness-shell:v2"

// shellConfig stands in for the reserved built-in a project seeds.
func shellConfig() *model.HarnessConfig {
	return &model.HarnessConfig{
		ID: "harness-shell", Slug: "shell", BuiltIn: true, Configured: true,
		Bundle: shellBundle, BundleDigest: contentDigest("shell-new"),
	}
}

func bundleTarget(ref, digest string) SandboxPinTarget {
	return SandboxPinTarget{Harness: HarnessPins{Bundle: model.BundlePin{Reference: ref, Digest: digest}}}
}

// TestSandboxUpgradeTargetIsSharedByBothPaths pins the rule that the read path
// and the upgrade mutation must agree on: a sandbox that reports an available
// upgrade must be one the upgrade call will actually move, and vice versa.
func TestSandboxUpgradeTargetIsSharedByBothPaths(t *testing.T) {
	shell := shellConfig()
	codexNew := contentDigest("codex-new")

	for _, tc := range []struct {
		name          string
		sandbox       *model.Sandbox
		config        *model.HarnessConfig
		wantTarget    SandboxPinTarget
		wantAvailable bool
	}{
		{
			// Not converged: the upgrade adopts the config, so it is available
			// regardless of digest (ADR 0032 §4).
			name:          "harnessless upgrades to the fallback",
			sandbox:       sandboxWithHarness("", shellBundle, contentDigest("shell-old")),
			config:        shell,
			wantTarget:    bundleTarget(shellBundle, shell.BundleDigest),
			wantAvailable: true,
		},
		{
			name:          "harnessless on the fallback's own digest is still unconverged",
			sandbox:       sandboxWithHarness("", shellBundle, shell.BundleDigest),
			config:        shell,
			wantTarget:    bundleTarget(shellBundle, shell.BundleDigest),
			wantAvailable: true,
		},
		{
			name:          "converged sandbox on the shell config is up to date",
			sandbox:       sandboxWithHarness("harness-shell", shellBundle, shell.BundleDigest),
			config:        shell,
			wantTarget:    bundleTarget(shellBundle, shell.BundleDigest),
			wantAvailable: false,
		},
		{
			name:          "harnessed upgrades to its config's bundle",
			sandbox:       sandboxWithHarness("harness-1", "discobox-harness-codex:v1", contentDigest("codex-old")),
			config:        &model.HarnessConfig{Bundle: "discobox-harness-codex:v1", BundleDigest: codexNew},
			wantTarget:    bundleTarget("discobox-harness-codex:v1", codexNew),
			wantAvailable: true,
		},
		{
			name:       "no config at all has nothing to move to",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:v1", contentDigest("codex-old")),
			wantTarget: SandboxPinTarget{},
		},
		{
			name:       "a config with no digest cannot be compared",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:v1", contentDigest("codex-old")),
			config:     &model.HarnessConfig{Bundle: "discobox-harness-codex:v1"},
			wantTarget: SandboxPinTarget{},
		},
		{
			// A development bundle no pool has built carries its reference
			// in place of a digest; no box can fetch that.
			name:       "a bundle with no content digest yet is nothing to move to",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:v1", contentDigest("codex-old")),
			config:     &model.HarnessConfig{Bundle: "discobox-harness-codex:dev-0123", BundleDigest: "discobox-harness-codex:dev-0123"},
			wantTarget: SandboxPinTarget{},
		},
		{
			// A config registered before bundles has no bundle, which pins
			// nothing.
			name:       "a config from before bundles is nothing to move to",
			sandbox:    sandboxWithHarness("harness-1", "", ""),
			config:     &model.HarnessConfig{RunCommand: []string{"codex"}},
			wantTarget: SandboxPinTarget{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, available := SandboxUpgradeTarget(tc.sandbox, tc.config, ReleasePins{})
			if target != tc.wantTarget || available != tc.wantAvailable {
				t.Fatalf("target = %+v available = %v, want %+v / %v", target, available, tc.wantTarget, tc.wantAvailable)
			}
			// The rendered field must agree with the rule it is rendered from.
			// The read path reads the preloaded association for a converged
			// sandbox and the passed fallback for one that is not, which is the
			// seam these two paths drifted across before they shared a rule.
			if tc.sandbox.HarnessConfigID != nil {
				tc.sandbox.HarnessConfig = tc.config
			}
			rendered := SandboxUpgrade(tc.sandbox, SandboxUpgradeBasis{Fallback: tc.config})
			if tc.wantTarget == (SandboxPinTarget{}) {
				if rendered != nil {
					t.Fatalf("rendered %v for a sandbox with nothing to move to", rendered)
				}
				return
			}
			if rendered == nil {
				t.Fatal("rendered no upgrade for a sandbox that has a target")
			}
			if rendered["available"] != tc.wantAvailable {
				t.Fatalf("rendered available = %v, want %v", rendered["available"], tc.wantAvailable)
			}
			if rendered["targetBundleDigest"] != tc.wantTarget.Harness.Bundle.Digest {
				t.Fatalf("rendered target digest = %v, want %v", rendered["targetBundleDigest"], tc.wantTarget.Harness.Bundle.Digest)
			}
			if _, ok := rendered["targetImageDigest"]; ok {
				t.Fatalf("rendered a target image %v for a bundle harness", rendered["targetImageDigest"])
			}
		})
	}
}

// TestSandboxUpgradeTargetIgnoresConfigMode: a config-mode sandbox runs the
// configure flow and is thrown away.
func TestSandboxUpgradeTargetIgnoresConfigMode(t *testing.T) {
	sb := sandboxWithHarness("", shellBundle, contentDigest("shell-old"))
	sb.HarnessMode = "config"
	if target, available := SandboxUpgradeTarget(sb, shellConfig(), ReleasePins{}); target != (SandboxPinTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
}

// TestSandboxUpgradeWithoutAFallbackConfig: seeding may not have created the
// `shell` config — a bundle that could not be inspected — and then an
// unconverged sandbox has nowhere to go yet.
func TestSandboxUpgradeWithoutAFallbackConfig(t *testing.T) {
	sb := sandboxWithHarness("", shellBundle, contentDigest("shell-old"))
	if target, available := SandboxUpgradeTarget(sb, nil, ReleasePins{}); target != (SandboxPinTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
	if rendered := SandboxUpgrade(sb, SandboxUpgradeBasis{}); rendered != nil {
		t.Fatalf("rendered %v with no fallback config", rendered)
	}
}

// A harness names no platform, so its bundle is something to move to
// whatever platform the sandbox runs on (ADR 26-11-01-122).
func TestSandboxUpgradeTargetIgnoresTheSandboxsPlatform(t *testing.T) {
	config := shellConfig()
	sb := sandboxWithHarness(config.ID, shellBundle, contentDigest("shell-old"))
	sb.Platform = platform.Platform{OS: "linux", Arch: "riscv64"}
	if target, available := SandboxUpgradeTarget(sb, config, ReleasePins{}); target.Harness.Bundle.Digest != config.BundleDigest || !available {
		t.Fatalf("target = %+v, available = %t; want the config's bundle", target, available)
	}
}

// The API answers with the sandbox's platform, so a client can tell what a
// discobox runs on, and says nothing for a sandbox that has none recorded yet.
func TestSandboxToAPIIncludesThePlatform(t *testing.T) {
	sb := sandboxWithHarness("", shellBundle, contentDigest("a"))
	sb.Platform = platform.Platform{OS: "linux", Arch: "arm64"}
	out, err := SandboxToAPI(sb, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Platform.Or(""); got != "linux/arm64" {
		t.Fatalf("platform = %q, want linux/arm64", got)
	}
	out, err = SandboxToAPI(sandboxWithHarness("", shellBundle, contentDigest("a")), SandboxUpgradeBasis{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Platform.IsSet() {
		t.Fatalf("platform = %q, want none for a sandbox with no platform recorded", out.Platform.Value)
	}
}

// The bundles are what a sandbox upgrades (ADR 26-10-16-423 §7): a harness
// bundle or a platform bundle that moved makes an upgrade available on its
// own, an upgrade writes the harness's pins, and a platform nobody has
// resolved leaves the sandbox's own in place rather than counting as moved.
func TestSandboxUpgradeTargetFollowsTheBundles(t *testing.T) {
	config := &model.HarnessConfig{
		Bundle: "ghcr.io/discobox-ai/discobox-harness-codex:v1", BundleDigest: contentDigest("bundle-new"),
	}
	platform := model.BundlePin{Reference: "ghcr.io/discobox-ai/discobox-platform:v1", Digest: contentDigest("platform-new")}
	current := func() *model.Sandbox {
		sb := sandboxWithHarness("harness-1", config.Bundle, config.BundleDigest)
		sb.PlatformBundle, sb.PlatformBundleDigest = platform.Reference, platform.Digest
		return sb
	}
	for _, tc := range []struct {
		name       string
		sandbox    func(*model.Sandbox)
		platform   model.BundlePin
		wantReason any
	}{
		{name: "nothing moved", platform: platform},
		{name: "the harness bundle moved", sandbox: func(sb *model.Sandbox) { sb.BundleDigest = contentDigest("bundle-old") }, platform: platform, wantReason: "bundleDigestChanged"},
		{name: "a sandbox pinned to no bundle", sandbox: func(sb *model.Sandbox) { sb.Bundle, sb.BundleDigest = "", "" }, platform: platform, wantReason: "bundleDigestChanged"},
		{
			// A sandbox from before bundles carries the runnable image it was
			// created on and no bundle: its harness's bundle is a move, and
			// the upgrade leaves that image behind.
			name: "a sandbox on a runnable harness image",
			sandbox: func(sb *model.Sandbox) {
				sb.Bundle, sb.BundleDigest = "", ""
				sb.Image, sb.ImageDigest = "discobox-harness-codex:old", contentDigest("image")
			},
			platform: platform, wantReason: "bundleDigestChanged",
		},
		{name: "the platform moved", sandbox: func(sb *model.Sandbox) { sb.PlatformBundleDigest = contentDigest("platform-old") }, platform: platform, wantReason: "platformBundleDigestChanged"},
		{name: "a platform not resolved is not a move", sandbox: func(sb *model.Sandbox) { sb.PlatformBundleDigest = contentDigest("platform-old") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := current()
			if tc.sandbox != nil {
				tc.sandbox(sb)
			}
			target, available := SandboxUpgradeTarget(sb, config, ReleasePins{Platform: tc.platform})
			if available != (tc.wantReason != nil) {
				t.Fatalf("available = %v, want %v", available, tc.wantReason != nil)
			}
			sb.HarnessConfig = config
			rendered := SandboxUpgrade(sb, SandboxUpgradeBasis{Release: ReleasePins{Platform: tc.platform}})
			if rendered["reason"] != tc.wantReason {
				t.Fatalf("reason = %v, want %v", rendered["reason"], tc.wantReason)
			}
			previousPlatform := sb.PlatformBundleDigest
			target.Apply(sb)
			if sb.Image != "" || sb.ImageDigest != "" || sb.Bundle != config.Bundle || sb.BundleDigest != config.BundleDigest {
				t.Fatalf("applied pins = %q %q %q %q, want the config's bundle alone", sb.Image, sb.ImageDigest, sb.Bundle, sb.BundleDigest)
			}
			wantPlatform := tc.platform.Digest
			if wantPlatform == "" {
				wantPlatform = previousPlatform
			}
			if sb.PlatformBundleDigest != wantPlatform {
				t.Fatalf("applied platform = %q, want %q", sb.PlatformBundleDigest, wantPlatform)
			}
		})
	}
}

// The platform is every box's (ADR 26-10-16-423 §7): a sandbox whose harness
// has nothing to move to still follows it, without its harness pin being
// touched, and a config-mode sandbox follows nothing.
func TestThePlatformIsATargetWithoutAHarnessOne(t *testing.T) {
	platform := model.BundlePin{Reference: "ghcr.io/discobox-ai/discobox-platform:v1", Digest: contentDigest("platform-new")}
	sb := sandboxWithHarness("harness-1", "discobox-harness-codex:v1", contentDigest("codex-old"))
	sb.PlatformBundleDigest = contentDigest("platform-old")
	target, available := SandboxUpgradeTarget(sb, nil, ReleasePins{Platform: platform})
	if !available || target.Harness != (HarnessPins{}) || target.Platform != platform {
		t.Fatalf("target = %+v available = %v, want the platform alone", target, available)
	}
	if rendered := SandboxUpgrade(sb, SandboxUpgradeBasis{Release: ReleasePins{Platform: platform}}); rendered["reason"] != "platformBundleDigestChanged" {
		t.Fatalf("rendered %v, want the platform's upgrade", rendered)
	}
	target.Apply(sb)
	if sb.BundleDigest != contentDigest("codex-old") || sb.PlatformBundleDigest != platform.Digest {
		t.Fatalf("applied bundle %q platform %q, want the bundle kept and the platform moved", sb.BundleDigest, sb.PlatformBundleDigest)
	}
	sb.HarnessMode = "config"
	if target, available := SandboxUpgradeTarget(sb, nil, ReleasePins{Platform: platform}); !target.Empty() || available {
		t.Fatalf("config-mode target = %+v available = %v, want nothing", target, available)
	}
}
