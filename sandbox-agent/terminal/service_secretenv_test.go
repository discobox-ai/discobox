package terminal

import (
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

func sentinels() map[string]string {
	//nolint:gosec // Sentinels, which are non-secret by construction.
	return map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "sentinel-oauth",
		"ANTHROPIC_API_KEY":       "sentinel-key",
		"HAND_BOUND":              "sentinel-hand",
	}
}

// A file-delivered credential must not reach the harness's environment. Claude
// Code prefers CLAUDE_CODE_OAUTH_TOKEN over the credentials file, and a token
// arriving that way carries no scopes — so leaving it exported silently limits
// the harness to inference no matter what the file says.
func TestExportedSecretEnvWithholdsFileDeliveredSecrets(t *testing.T) {
	s := &Service{secretEnv: sentinels, fileSecrets: []string{"CLAUDE_CODE_OAUTH_TOKEN"}}
	got := s.exportedSecretEnv()
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("file-delivered secret was exported: %v", got)
	}
	if got["ANTHROPIC_API_KEY"] != "sentinel-key" {
		t.Fatalf("env-delivered secret was dropped: %v", got)
	}
	// Undeclared secrets are nobody's to withhold.
	if got["HAND_BOUND"] != "sentinel-hand" {
		t.Fatalf("hand-bound secret was dropped: %v", got)
	}
	// The sentinel still exists for the file template to render.
	if s.secretEnv()["CLAUDE_CODE_OAUTH_TOKEN"] != "sentinel-oauth" {
		t.Fatal("withholding the export must not remove the sentinel itself")
	}
}

func TestExportedSecretEnvIsUnchangedWithoutDeclarations(t *testing.T) {
	s := &Service{secretEnv: sentinels}
	if len(s.exportedSecretEnv()) != 3 {
		t.Fatalf("exported = %v, want every sentinel", s.exportedSecretEnv())
	}
}

// The declaration reaches the service from the harness contract, so a harness
// that marks a secret file-delivered gets it withheld without anything else
// being configured.
func TestServiceTakesFileSecretsFromTheHarness(t *testing.T) {
	dir := t.TempDir()
	manager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Units:       &fakeUnits{},
	})
	if err != nil {
		t.Fatalf("exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       manager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Units:       &fakeUnits{},
		Installer:   &noopInstaller{},
		SecretEnv:   sentinels,
		Harness: config.Harness{
			ID: "claude", Name: "Claude", Command: []string{"claude"},
			FileSecrets: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	if _, ok := svc.exportedSecretEnv()["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatal("harness declaration did not reach the env filter")
	}
}
