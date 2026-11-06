package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPoolOutboundEnvironmentEmptyOverridesYAML(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "server.yaml")
	body := "pools:\n  proxy:\n    mode: explicit\n    http: http://global:3128\n    https: http://secure:3128\n  overrides:\n    pool_east:\n      proxy:\n        mode: explicit\n        http: http://east:3128\n"
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigFileVar, file)
	t.Setenv("DISCOBOX_POOLS_PROXY_HTTP", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PoolOutbound.Global.HTTP != "" || cfg.PoolOutbound.Global.HTTPS != "http://secure:3128" {
		t.Fatal("empty environment did not replace YAML field")
	}
	if got := cfg.PoolOutbound.ByPool["pool_east"]; got.HTTP != "http://east:3128" || got.HTTPS != "" {
		t.Fatal("global environment leaked into a pool override")
	}
}

func TestPoolDirectRejectsEvenEmptyExplicitPayload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(file, []byte("pools:\n  overrides:\n    pool_east:\n      proxy:\n        mode: direct\n        http: \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ConfigFileVar, file)
	if _, err := Load(); err == nil {
		t.Fatal("ambiguous pool override accepted")
	}
}
