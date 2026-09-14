package config

import (
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/releasemanifest"
)

func TestReleaseManifestOverridesDevelopmentImageSettings(t *testing.T) {
	clearConfigEnv(t)
	path, err := filepath.Abs("../../../releasemanifest/examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(releasemanifest.Env, path)
	t.Setenv("DISCOBOX_DOCKER_POOL_IMAGE", "pool:local")
	t.Setenv("DISCOBOX_DEFAULT_SANDBOX_IMAGE", "sandbox:local")
	t.Setenv("DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST", "stale")
	t.Setenv("DISCOBOX_HARNESS_CODEX_IMAGE", "codex:local")
	t.Setenv("DISCOBOX_DEV_DOCKER_IMAGE_SYNC", "true")
	t.Setenv("DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST", "does-not-exist")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Release
	if m == nil || cfg.DockerPoolImage != m.Images.PoolAgent || cfg.DefaultSandboxImage != m.Images.SandboxAgent || cfg.HarnessImages["codex"] != m.Images.Harnesses["codex"] {
		t.Fatalf("release images were not applied: %+v", cfg)
	}
	if cfg.DevImageSync || cfg.DevImageManifest != "" || cfg.DefaultSandboxImageDigest != "" {
		t.Fatal("stale development configuration survived")
	}
}

func TestBadReleaseManifestFailsInsteadOfFallingBack(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(releasemanifest.Env, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := Load(); err == nil {
		t.Fatal("silently ignored manifest")
	}
}
