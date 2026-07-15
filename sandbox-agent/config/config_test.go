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

func TestLoadUsesSelectedHarnessOnlyAsImageOverlay(t *testing.T) {
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
			"harnessMode": "config",
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
			"files": [{"path": ".claude.json", "content": "{}"}]
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ResolvedHarnessConfig == nil || cfg.ResolvedHarnessConfig.ID != "claude" || len(cfg.ResolvedHarnessConfig.Files) != 1 {
		t.Fatalf("resolved harness = %#v, want claude file overlay", cfg.ResolvedHarnessConfig)
	}
	if cfg.HarnessMode != "config" {
		t.Fatalf("harness mode = %q, want config", cfg.HarnessMode)
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
	source, ok := cfg.SandboxConfig["source"].(map[string]any)
	if !ok {
		t.Fatalf("sandbox config = %#v, want public source object", cfg.SandboxConfig)
	}
	destination, ok := source["destination"].(map[string]any)
	if !ok || destination["workingDirectory"] != "/workspace/project" {
		t.Fatalf("sandbox config source = %#v, want public lower-camel-case fields", source)
	}
}
