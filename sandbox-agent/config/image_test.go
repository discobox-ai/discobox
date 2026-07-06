package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if err := os.WriteFile(path, []byte(`{"env":{"PATH":"/usr/bin"}}`), 0600); err != nil {
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
