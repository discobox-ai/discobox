package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadsIdentityFromFileOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-file",
		"provider": {
			"kind": "discobox-pool",
			"projectId": "project-file",
			"poolId": "pool-file",
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
	if cfg.Identity.ProjectID != "project-file" {
		t.Fatalf("project id = %q", cfg.Identity.ProjectID)
	}
	if cfg.Identity.SandboxID != "sandbox-file" {
		t.Fatalf("sandbox id = %q", cfg.Identity.SandboxID)
	}
	if cfg.ListenAddress != ":3003" {
		t.Fatalf("listen address = %q", cfg.ListenAddress)
	}
}

func TestLoadDecodesTheOneResolvedHarness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-1",
		"provider": {
			"kind": "discobox-pool",
			"projectId": "project-1",
			"poolId": "pool-1",
			"publicKeys": {
				"controlPlane": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"
			}
		},
		"harnessMode": "config",
		"env": {
			"BASE": "sandbox"
		},
		"prompt": ["fix", "the bug"],
		"harness": {
			"id": "claude",
			"name": "Claude",
			"runCommand": ["claude"]
		},
		"files": [{"path": ".claude.json", "content": "{}"}]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Harness.ID != "claude" || len(cfg.Harness.Files) != 1 {
		t.Fatalf("harness = %#v, want claude with one file", cfg.Harness)
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

func TestLoadDerivesExecDefaultsFromEffectiveConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{
		"apiVersion": "discobox.dev/sandbox/v1",
		"sandboxId": "sandbox-1",
		"provider": {
			"kind": "discobox-pool",
			"projectId": "project-1",
			"poolId": "pool-1",
			"publicKeys": {
				"controlPlane": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"
			}
		},
		"sources": [
			{"slug": "primary", "target": "/workspace/project"}
		],
		"user": {
			"name": "darren",
			"uid": 1000,
			"gid": 1001,
			"homeDirectory": "/home/darren"
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
	sources, ok := cfg.SandboxConfig["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("sandbox config sources = %#v, want the raw sources array as template data", cfg.SandboxConfig["sources"])
	}
}
