package terminal

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
)

func TestFileInstallerKeepsSettingsLiteral(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home}
	harness := config.Harness{ID: "literal", Files: []config.HarnessFile{{Path: "config", Content: `{{ .name }} ${NAME} {"$secret":"TOKEN"}`}}}
	if err := installer.EnsureInstalled(context.Background(), harness, "", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != harness.Files[0].Content {
		t.Fatalf("literal changed: %q", data)
	}
}

func TestConfigureInstallerLeavesCredentialSeedingToTheHarness(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	installer.Configure = true
	h := credentialsHarness()
	if err := installer.EnsureInstalled(t.Context(), h, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, credentialsPath)); !os.IsNotExist(err) {
		t.Fatalf("configure installed a run credential: %v", err)
	}
}
