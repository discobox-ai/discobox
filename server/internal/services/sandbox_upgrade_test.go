package services

import (
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func sandboxWithHarness(configID string, image, digest string) *model.Sandbox {
	sb := &model.Sandbox{SandboxManifest: model.SandboxManifest{Image: image, ImageDigest: digest, HarnessMode: "run"}}
	if configID != "" {
		sb.HarnessConfigID = &configID
	}
	return sb
}

// TestSandboxUpgradeTargetIsSharedByBothPaths pins the rule that the read path
// and the upgrade mutation must agree on: a sandbox that reports an available
// upgrade must be one the upgrade call will actually move, and vice versa.
func TestSandboxUpgradeTargetIsSharedByBothPaths(t *testing.T) {
	def := SandboxImageTarget{Image: "discobox-sandbox-agent:new", Digest: "sha256:new"}

	for _, tc := range []struct {
		name          string
		sandbox       *model.Sandbox
		config        *model.HarnessConfig
		wantTarget    SandboxImageTarget
		wantAvailable bool
	}{
		{
			// No harness config is a choice of image, not the absence of one.
			name:          "harnessless upgrades to the default image",
			sandbox:       sandboxWithHarness("", "discobox-sandbox-agent:old", "sha256:old"),
			wantTarget:    def,
			wantAvailable: true,
		},
		{
			name:          "harnessless already on the default image",
			sandbox:       sandboxWithHarness("", "discobox-sandbox-agent:new", "sha256:new"),
			wantTarget:    def,
			wantAvailable: false,
		},
		{
			name:          "harnessed upgrades to its config's image",
			sandbox:       sandboxWithHarness("harness-1", "discobox-harness-codex:old", "sha256:codex-old"),
			config:        &model.HarnessConfig{Image: "discobox-harness-codex:new", ImageDigest: "sha256:codex-new"},
			wantTarget:    SandboxImageTarget{Image: "discobox-harness-codex:new", Digest: "sha256:codex-new"},
			wantAvailable: true,
		},
		{
			// Its harness config is gone, so the default is not its answer: it
			// was never running the default image.
			name:       "harnessed with a missing config has nothing to move to",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:old", "sha256:codex-old"),
			wantTarget: SandboxImageTarget{},
		},
		{
			name:       "a config with no digest cannot be compared",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:old", "sha256:codex-old"),
			config:     &model.HarnessConfig{Image: "discobox-harness-codex:new"},
			wantTarget: SandboxImageTarget{},
		},
		{
			name:          "an unpinned sandbox is eligible, not excluded",
			sandbox:       sandboxWithHarness("", "discobox-sandbox-agent:old", ""),
			wantTarget:    def,
			wantAvailable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, available := SandboxUpgradeTarget(tc.sandbox, tc.config, def)
			if target != tc.wantTarget || available != tc.wantAvailable {
				t.Fatalf("target = %+v available = %v, want %+v / %v", target, available, tc.wantTarget, tc.wantAvailable)
			}
			// The rendered field must agree with the rule it is rendered from.
			// The read path reads the preloaded association rather than
			// loading the config itself, which is the seam these two paths
			// drifted across before they shared a rule.
			tc.sandbox.HarnessConfig = tc.config
			rendered := SandboxUpgrade(tc.sandbox, def)
			if tc.wantTarget == (SandboxImageTarget{}) {
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
			if rendered["targetImageDigest"] != tc.wantTarget.Digest {
				t.Fatalf("rendered target digest = %v, want %v", rendered["targetImageDigest"], tc.wantTarget.Digest)
			}
		})
	}
}

// TestSandboxUpgradeTargetIgnoresConfigMode: a config-mode sandbox runs the
// configure command against a deliberately fixed image.
func TestSandboxUpgradeTargetIgnoresConfigMode(t *testing.T) {
	sb := sandboxWithHarness("", "discobox-sandbox-agent:old", "sha256:old")
	sb.HarnessMode = "config"
	def := SandboxImageTarget{Image: "discobox-sandbox-agent:new", Digest: "sha256:new"}
	if target, available := SandboxUpgradeTarget(sb, nil, def); target != (SandboxImageTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
}

// TestSandboxUpgradeNeedsAKnownDefaultDigest: with no digest for the default
// image there is nothing to compare, and comparing tags would report "up to
// date" for every sandbox on a tag its workflow rebuilds in place.
func TestSandboxUpgradeNeedsAKnownDefaultDigest(t *testing.T) {
	sb := sandboxWithHarness("", "discobox-sandbox-agent:local", "sha256:old")
	def := SandboxImageTarget{Image: "discobox-sandbox-agent:local"}
	if target, available := SandboxUpgradeTarget(sb, nil, def); target != (SandboxImageTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
}
