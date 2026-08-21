package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
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
			"runCommand": ["claude"],
			"configCommand": ["configure-claude"]
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
	if len(cfg.Harness.Command) != 1 || cfg.Harness.Command[0] != "configure-claude" {
		t.Fatalf("harness command = %#v, want config command", cfg.Harness.Command)
	}
	if cfg.Env["BASE"] != "sandbox" {
		t.Fatalf("env = %#v, want sandbox config env", cfg.Env)
	}
	if len(cfg.Prompt) != 2 || cfg.Prompt[0] != "fix" || cfg.Prompt[1] != "the bug" {
		t.Fatalf("prompt = %#v, want [fix, the bug]", cfg.Prompt)
	}
}

func TestConfigFromEffectiveSelectsHarnessCommandForMode(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
		want string
	}{
		{name: "run", mode: "run", want: "run-harness"},
		{name: "config", mode: "config", want: "configure-harness"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := configFromEffective(sandboxconfig.Config{
				HarnessMode: test.mode,
				Harness: sandboxconfig.Harness{
					ID:            "test",
					RunCommand:    []string{"run-harness"},
					ConfigCommand: []string{"configure-harness"},
				},
			})
			if len(cfg.Harness.Command) != 1 || cfg.Harness.Command[0] != test.want {
				t.Fatalf("harness command = %#v, want %q", cfg.Harness.Command, test.want)
			}
		})
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

// validTestConfig is the minimum a sandbox manifest must carry to validate.
func validTestConfig() Config {
	return Config{
		Identity:              Identity{ProjectID: "proj_test000000001", SandboxID: "sbx_test0000000001"},
		ControlPlanePublicKey: "ssh-ed25519 AAAAtest",
	}
}

// TestValidateAcceptsACommandlessHarness: the control plane can name a harness
// without naming a command, which means the run user's login shell — the only
// thing the sandbox itself can resolve (ADR 0025 §2).
func TestValidateAcceptsACommandlessHarness(t *testing.T) {
	cfg := validTestConfig()
	cfg.Harness = Harness{ID: "harness_shell0000001", Name: "Shell"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate commandless harness: %v", err)
	}
}

// TestValidateRejectsABlankHarnessCommand keeps the malformed case rejected: a
// declared command that says nothing is not the same as declaring none.
func TestValidateRejectsABlankHarnessCommand(t *testing.T) {
	cfg := validTestConfig()
	cfg.Harness = Harness{ID: "harness_shell0000001", Name: "Shell", Command: []string{"  "}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a blank harness command to be rejected")
	}
}
