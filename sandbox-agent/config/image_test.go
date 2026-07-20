package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/harness"
)

func TestResolveVolumesExpandsTokens(t *testing.T) {
	const doc = `{
	  "apiVersion": "discobox.dev/image/v1",
	  "volumes": [
	    { "path": "%HOME%", "volume": "data", "uid": "%UID%", "gid": "%GID%", "mode": "0755" },
	    { "path": "/var/lib/docker", "volume": "data", "uid": 0, "gid": 0, "mode": "0711" },
	    { "path": "/var/lib/discobox/pnpm", "volume": "cache" }
	  ]
	}`
	var cfg ImageConfig
	if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal image config: %v", err)
	}
	resolved, err := cfg.ResolveVolumes(VolumeRuntime{Home: "/home/dev", UID: 1000, GID: 2000})
	if err != nil {
		t.Fatalf("resolve volumes: %v", err)
	}
	if len(resolved) != 3 {
		t.Fatalf("resolved %d volumes, want 3", len(resolved))
	}
	home := resolved[0]
	if home.Path != "/home/dev" || home.Kind != VolumeData {
		t.Fatalf("home = %#v", home)
	}
	if home.UID == nil || *home.UID != 1000 || home.GID == nil || *home.GID != 2000 {
		t.Fatalf("home uid/gid = %v/%v, want 1000/2000", home.UID, home.GID)
	}
	if home.Mode == nil || *home.Mode != 0o755 {
		t.Fatalf("home mode = %v, want 0755", home.Mode)
	}
	docker := resolved[1]
	if docker.UID == nil || *docker.UID != 0 || docker.Mode == nil || *docker.Mode != 0o711 {
		t.Fatalf("docker = %#v", docker)
	}
	cache := resolved[2]
	if cache.Kind != VolumeCache || cache.UID != nil || cache.GID != nil || cache.Mode != nil {
		t.Fatalf("cache = %#v, want unset ownership", cache)
	}
}

func TestResolveVolumesRejectsBadInput(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown kind":  `{"volumes":[{"path":"/a","volume":"scratch"}]}`,
		"relative path": `{"volumes":[{"path":"a/b","volume":"data"}]}`,
		"bad mode":      `{"volumes":[{"path":"/a","volume":"data","mode":"garbage"}]}`,
		"bad uid value": `{"volumes":[{"path":"/a","volume":"data","uid":"%NOPE%"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var cfg ImageConfig
			if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, err := cfg.ResolveVolumes(VolumeRuntime{Home: "/home/dev"}); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

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
	if len(normal.RelaunchCommand) != 2 || normal.RelaunchCommand[1] != "--continue" {
		t.Fatalf("normal relaunch = %#v, want the image's relaunch command", normal.RelaunchCommand)
	}
	configured, ok, err := image.HarnessForMode("config")
	if err != nil || !ok || configured.Command[0] != "configure-claude" {
		t.Fatalf("config harness = %#v, ok=%t, err=%v", configured, ok, err)
	}
	// Relaunch is untouched by config mode: the terminal service forces the config
	// command there and never consults it.
	if len(configured.RelaunchCommand) != 2 || configured.RelaunchCommand[1] != "--continue" {
		t.Fatalf("config relaunch = %#v, want the image's relaunch command", configured.RelaunchCommand)
	}
}

func TestIncludedHarnessImagesSupportConfigMode(t *testing.T) {
	for name, dir := range map[string]string{
		"codex":       "codex-cli",
		"claude-code": "claude-code",
		"opencode":    "opencode",
	} {
		image, err := LoadImage(filepath.Join("..", "..", "harness", dir, "image.json"))
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
