package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/releasemanifest"
)

// releaseWithBundles writes v0.8.0's manifest with a bundle named for its
// platform and each of its harnesses, as a release from bundles on has, and
// what a pool's box is made of in place of its pool-agent image, as a release
// from a pool being a box on has.
func releaseWithBundles(t *testing.T) string {
	t.Helper()
	m, err := releasemanifest.Read("../../../pkg/releasemanifest/examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	m.Bundles = releasemanifest.Bundles{
		Platform: "ghcr.io/discobox-ai/discobox-platform:v0.8.0", Harnesses: map[string]string{},
		Agent:     "ghcr.io/discobox-ai/discobox-agent:v0.8.0",
		PoolAgent: "ghcr.io/discobox-ai/discobox-pool-agent-bundle:v0.8.0",
	}
	m.Images.PoolAgent, m.Images.SandboxBase = "", "ghcr.io/discobox-ai/discobox-sandbox-base:v0.8.0"
	for slug := range m.Images.Harnesses {
		m.Bundles.Harnesses[slug] = "ghcr.io/discobox-ai/discobox-harness-" + slug + ":v0.8.0"
	}
	m.Images.SandboxAgent, m.Images.Harnesses = "", nil
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseManifestOverridesDevelopmentImageSettings(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(releasemanifest.Env, releaseWithBundles(t))
	t.Setenv("DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE", "base:local")
	t.Setenv("DISCOBOX_AGENT_BUNDLE", "agent:local")
	t.Setenv("DISCOBOX_HARNESS_CODEX_BUNDLE", "codex-bundle:local")
	t.Setenv("DISCOBOX_PLATFORM_BUNDLE", "platform:local")
	t.Setenv("DISCOBOX_POOL_AGENT_BUNDLE", "pool-agent-bundle:local")
	t.Setenv("DISCOBOX_DEV_DOCKER_IMAGE_SYNC", "true")
	t.Setenv("DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST", "does-not-exist")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Release
	if m == nil || cfg.DockerSandboxBaseImage != m.Images.SandboxBase || cfg.AgentBundle != m.Bundles.Agent || cfg.PoolAgentBundle != m.Bundles.PoolAgent {
		t.Fatalf("release images were not applied: %+v", cfg)
	}
	if cfg.Harnesses.PlatformBundle != m.Bundles.Platform || cfg.Harnesses.Bundles["codex"] != m.Bundles.Harnesses["codex"] {
		t.Fatalf("release bundles were not applied: %+v", cfg.Harnesses)
	}
	if cfg.PoolAgentBundle != m.Bundles.PoolAgent {
		t.Fatalf("pool-agent bundle = %q, want the release's %q", cfg.PoolAgentBundle, m.Bundles.PoolAgent)
	}
	if cfg.DevImageSync || cfg.DevImageManifest != "" {
		t.Fatal("stale development configuration survived")
	}
}

// A release from before bundles is a valid manifest the CLI reads for an
// older server, and one this server cannot run: it has no manifest to read
// any harness's from.
func TestAReleaseFromBeforeBundlesIsRefused(t *testing.T) {
	clearConfigEnv(t)
	path, err := filepath.Abs("../../../pkg/releasemanifest/examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(releasemanifest.Env, path)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "names no bundles") {
		t.Fatalf("Load() = %v, want a release from before bundles refused", err)
	}
}

// A release with bundles from before a pool was a box runs a Docker pool's
// container from its pool-agent image, which this server no longer runs, and
// names no agent bundle to place in the box it makes instead.
func TestAReleaseFromBeforeAPoolWasABoxIsRefused(t *testing.T) {
	clearConfigEnv(t)
	path := releaseWithBundles(t)
	m, err := releasemanifest.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Images.PoolAgent, m.Images.SandboxBase, m.Bundles.Agent = "ghcr.io/discobox-ai/discobox-pool-agent:v0.8.0", "", ""
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(releasemanifest.Env, path)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "before a pool was a box") {
		t.Fatalf("Load() = %v, want a release from before a pool was a box refused", err)
	}
}

// Without a release, the platform bundle is this build's unless configured,
// and each built-in's bundle may be overridden from the environment.
func TestHarnessReferencesComeFromTheEnvironment(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harnesses.PlatformBundle != harness.PlatformBundleRef() {
		t.Fatalf("platform bundle = %q, want this build's %q", cfg.Harnesses.PlatformBundle, harness.PlatformBundleRef())
	}
	t.Setenv("DISCOBOX_PLATFORM_BUNDLE", "discobox-platform:dev-1")
	t.Setenv("DISCOBOX_HARNESS_CODEX_BUNDLE", "discobox-harness-codex:dev-1")
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.Harnesses.PlatformBundle != "discobox-platform:dev-1" || cfg.Harnesses.Bundles["codex"] != "discobox-harness-codex:dev-1" {
		t.Fatalf("references = %+v, want the environment's", cfg.Harnesses)
	}
}

func TestBadReleaseManifestFailsInsteadOfFallingBack(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(releasemanifest.Env, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := Load(); err == nil {
		t.Fatal("silently ignored manifest")
	}
}
