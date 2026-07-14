package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-file",
		"config": {
			"name": "test",
			"image": "image",
			"cpuVcpus": 1,
			"memoryBytes": 1024,
			"storageBytes": 2048
		},
		"provider": {
			"kind": "discobox-worker",
			"projectId": "project-file",
			"workerId": "worker-file",
			"publicKeys": {
				"controlPlane": "file-key"
			}
		}
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

func TestLoadUsesHarnessConfigsAsLaunchableAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-1",
		"config": {
			"name": "test",
			"image": "image",
			"cpuVcpus": 1,
			"memoryBytes": 1024,
			"storageBytes": 2048,
			"env": {
				"BASE": "sandbox"
			},
			"prompt": ["fix", "the bug"]
		},
		"provider": {
			"kind": "discobox-worker",
			"projectId": "project-1",
			"workerId": "worker-1",
			"publicKeys": {
				"controlPlane": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"
			}
		},
		"resolvedHarnessConfig": {
			"id": "claude",
			"name": "Claude",
			"runCommand": ["claude"]
		},
		"harnessConfigs": [
			{
				"id": "codex",
				"name": "Codex",
				"installCommand": ["npm", "install", "-g", "@openai/codex"],
				"runCommand": ["codex"],
				"relaunchCommand": ["codex", "resume", "--last"],
				"isDefault": true
			},
			{
				"id": "claude",
				"name": "Claude",
				"runCommand": ["claude"]
			}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Harnesses) != 2 {
		t.Fatalf("harnesses = %#v, want 2", cfg.Harnesses)
	}
	if cfg.Harnesses[0].ID != "codex" || !cfg.Harnesses[0].IsDefault || len(cfg.Harnesses[0].InstallCommand) == 0 {
		t.Fatalf("first harness = %#v, want default codex with install command", cfg.Harnesses[0])
	}
	if got := cfg.Harnesses[0].RelaunchCommand; len(got) != 3 || got[0] != "codex" || got[1] != "resume" || got[2] != "--last" {
		t.Fatalf("codex relaunch command = %#v, want [codex, resume, --last]", got)
	}
	if cfg.Harnesses[1].ID != "claude" {
		t.Fatalf("second harness = %#v, want claude", cfg.Harnesses[1])
	}
	if cfg.Env["BASE"] != "sandbox" {
		t.Fatalf("env = %#v, want sandbox config env", cfg.Env)
	}
	if len(cfg.Prompt) != 2 || cfg.Prompt[0] != "fix" || cfg.Prompt[1] != "the bug" {
		t.Fatalf("prompt = %#v, want [fix, the bug]", cfg.Prompt)
	}
}

func TestLoadDerivesExecDefaultsFromSandboxManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-1",
		"config": {
			"name": "test",
			"image": "image",
			"cpuVcpus": 1,
			"memoryBytes": 1024,
			"storageBytes": 2048,
			"source": {
				"kind": "git",
				"destination": {
					"workingDirectory": "/workspace/project"
				}
			},
			"user": {
				"name": "darren",
				"uid": 1000,
				"gid": 1001,
				"homeDirectory": "/home/darren"
			}
		},
		"provider": {
			"kind": "discobox-worker",
			"projectId": "project-1",
			"workerId": "worker-1",
			"publicKeys": {
				"controlPlane": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ExecDefaults.Workdir != "/workspace/project" {
		t.Fatalf("exec default workdir = %q", cfg.ExecDefaults.Workdir)
	}
	if cfg.ExecDefaults.Username != "darren" || cfg.ExecDefaults.HomeDirectory != "/home/darren" || cfg.ExecDefaults.UID == nil || *cfg.ExecDefaults.UID != 1000 || cfg.ExecDefaults.GID == nil || *cfg.ExecDefaults.GID != 1001 {
		t.Fatalf("exec default user = %#v", cfg.ExecDefaults)
	}
}
