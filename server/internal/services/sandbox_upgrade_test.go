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

// shellConfig stands in for the reserved built-in a project seeds.
func shellConfig() *model.HarnessConfig {
	return &model.HarnessConfig{
		ID: "harness-shell", Slug: "shell", BuiltIn: true, Configured: true,
		Image: "discobox-sandbox-agent:new", ImageDigest: "sha256:new",
	}
}

// TestSandboxUpgradeTargetIsSharedByBothPaths pins the rule that the read path
// and the upgrade mutation must agree on: a sandbox that reports an available
// upgrade must be one the upgrade call will actually move, and vice versa.
func TestSandboxUpgradeTargetIsSharedByBothPaths(t *testing.T) {
	shell := shellConfig()

	for _, tc := range []struct {
		name          string
		sandbox       *model.Sandbox
		config        *model.HarnessConfig
		wantTarget    SandboxImageTarget
		wantAvailable bool
	}{
		{
			// Not converged: the upgrade adopts the config, so it is available
			// regardless of digest (ADR 0025 §4).
			name:          "harnessless upgrades to the fallback",
			sandbox:       sandboxWithHarness("", "discobox-sandbox-agent:old", "sha256:old"),
			config:        shell,
			wantTarget:    SandboxImageTarget{Image: "discobox-sandbox-agent:new", Digest: "sha256:new"},
			wantAvailable: true,
		},
		{
			name:          "harnessless on the fallback's own digest is still unconverged",
			sandbox:       sandboxWithHarness("", "discobox-sandbox-agent:new", "sha256:new"),
			config:        shell,
			wantTarget:    SandboxImageTarget{Image: "discobox-sandbox-agent:new", Digest: "sha256:new"},
			wantAvailable: true,
		},
		{
			name:          "converged sandbox on the shell config is up to date",
			sandbox:       sandboxWithHarness("harness-shell", "discobox-sandbox-agent:new", "sha256:new"),
			config:        shell,
			wantTarget:    SandboxImageTarget{Image: "discobox-sandbox-agent:new", Digest: "sha256:new"},
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
			name:       "no config at all has nothing to move to",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:old", "sha256:codex-old"),
			wantTarget: SandboxImageTarget{},
		},
		{
			name:       "a config with no digest cannot be compared",
			sandbox:    sandboxWithHarness("harness-1", "discobox-harness-codex:old", "sha256:codex-old"),
			config:     &model.HarnessConfig{Image: "discobox-harness-codex:new"},
			wantTarget: SandboxImageTarget{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, available := SandboxUpgradeTarget(tc.sandbox, tc.config)
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
			rendered := SandboxUpgrade(tc.sandbox, tc.config)
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
	if target, available := SandboxUpgradeTarget(sb, shellConfig()); target != (SandboxImageTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
}

// TestSandboxUpgradeWithoutAFallbackConfig: seeding may not have created the
// `shell` config — no default sandbox image, or an image that could not be
// inspected — and then an unconverged sandbox has nowhere to go yet.
func TestSandboxUpgradeWithoutAFallbackConfig(t *testing.T) {
	sb := sandboxWithHarness("", "discobox-sandbox-agent:old", "sha256:old")
	if target, available := SandboxUpgradeTarget(sb, nil); target != (SandboxImageTarget{}) || available {
		t.Fatalf("target = %+v available = %v, want nothing to move to", target, available)
	}
	if rendered := SandboxUpgrade(sb, nil); rendered != nil {
		t.Fatalf("rendered %v with no fallback config", rendered)
	}
}
