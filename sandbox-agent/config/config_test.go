package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-agent.json")
	if err := os.WriteFile(path, []byte(`{
		"identity": {
			"projectId": "project-file",
			"sandboxId": "sandbox-file",
			"workerId": "worker-file"
		},
		"controlPlanePublicKey": "file-key"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOBOX_PROJECT_ID", "project-env")
	t.Setenv("DISCOBOX_CONTROL_PLANE_PUBLIC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Identity.ProjectID != "project-env" {
		t.Fatalf("project id = %q", cfg.Identity.ProjectID)
	}
	if cfg.Identity.SandboxID != "sandbox-file" {
		t.Fatalf("sandbox id = %q", cfg.Identity.SandboxID)
	}
	if cfg.ListenAddress != ":3003" {
		t.Fatalf("listen address = %q", cfg.ListenAddress)
	}
}
