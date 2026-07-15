package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/harness"
)

func TestLoadImageMissingFileReturnsEmptyConfig(t *testing.T) {
	cfg, err := LoadImage(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("load missing image config: %v", err)
	}
	if len(cfg.Env) != 0 {
		t.Fatalf("env = %#v, want empty", cfg.Env)
	}
}

func TestHarnessForConfigModeUsesImageCommand(t *testing.T) {
	image := ImageConfig{Harness: &harness.Image{
		ID: "claude-code", Name: "Claude Code",
		RunCommand: []string{"claude"}, RelaunchCommand: []string{"claude", "--continue"},
		Config: &harness.ImageMode{Command: []string{"configure-claude"}},
	}}
	normal, ok, err := image.HarnessForMode("run")
	if err != nil || !ok || normal.Command[0] != "claude" {
		t.Fatalf("normal harness = %#v, ok=%t, err=%v", normal, ok, err)
	}
	configured, ok, err := image.HarnessForMode("config")
	if err != nil || !ok || configured.Command[0] != "configure-claude" {
		t.Fatalf("config harness = %#v, ok=%t, err=%v", configured, ok, err)
	}
}

func TestIncludedHarnessImagesSupportConfigMode(t *testing.T) {
	for _, name := range []string{"codex", "claude-code", "opencode"} {
		image, err := LoadImage(filepath.Join("..", "image", "harnesses", name, "image.json"))
		if err != nil {
			t.Fatalf("load %s image: %v", name, err)
		}
		configured, ok, err := image.HarnessForMode("config")
		if err != nil || !ok || len(configured.Command) == 0 {
			t.Fatalf("%s config harness = %#v, ok=%t, err=%v", name, configured, ok, err)
		}
	}
}

func TestApplyImageEnvDefaultsExpandsHomeAndPreservesOverrides(t *testing.T) {
	env := map[string]string{
		"HOME": "/home/darren",
		"PATH": "/custom/bin",
	}
	out := ApplyImageEnvDefaults(env, ImageConfig{Env: map[string]string{
		"NPM_CONFIG_PREFIX": "%HOME%/.npm-global",
		"PATH":              "%HOME%/.npm-global/bin:/usr/bin",
	}})

	if out["NPM_CONFIG_PREFIX"] != "/home/darren/.npm-global" {
		t.Fatalf("NPM_CONFIG_PREFIX = %q, want expanded user prefix", out["NPM_CONFIG_PREFIX"])
	}
	if out["PATH"] != "/custom/bin" {
		t.Fatalf("PATH = %q, want override preserved", out["PATH"])
	}
}

func TestLoadImageReadsEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.json")
	if err := os.WriteFile(path, []byte(`{"apiVersion":"discobox.dev/image/v1","env":{"PATH":"/usr/bin"}}`), 0600); err != nil {
		t.Fatalf("write image config: %v", err)
	}
	cfg, err := LoadImage(path)
	if err != nil {
		t.Fatalf("load image config: %v", err)
	}
	if cfg.Env["PATH"] != "/usr/bin" {
		t.Fatalf("PATH = %q, want /usr/bin", cfg.Env["PATH"])
	}
}
