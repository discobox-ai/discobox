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

func TestLoadUsesAgentConfigsAsLaunchableAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-agent.json")
	if err := os.WriteFile(path, []byte(`{
		"identity": {
			"projectId": "project-1",
			"sandboxId": "sandbox-1",
			"workerId": "worker-1"
		},
		"controlPlanePublicKey": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`",
		"resolvedAgentConfig": {
			"id": "claude",
			"name": "Claude",
			"command": ["/bin/bash", "-lc", "claude"]
		},
		"agentConfigs": [
			{
				"id": "codex",
				"name": "Codex",
				"installCommand": "npm install -g @openai/codex",
				"command": ["/bin/bash", "-lc", "codex"],
				"isDefault": true
			},
			{
				"id": "claude",
				"name": "Claude",
				"command": ["/bin/bash", "-lc", "claude"]
			}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents = %#v, want 2", cfg.Agents)
	}
	if cfg.Agents[0].ID != "codex" || !cfg.Agents[0].IsDefault || cfg.Agents[0].InstallCommand == "" {
		t.Fatalf("first agent = %#v, want default codex with install command", cfg.Agents[0])
	}
	if cfg.Agents[1].ID != "claude" {
		t.Fatalf("second agent = %#v, want claude", cfg.Agents[1])
	}
}
