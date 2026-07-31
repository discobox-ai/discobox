package terminal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	discoboxharness "github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandbox-agent/config"
)

// claudeImageHarness reads the Claude harness's authoring-time image.json
// directly (the runtime carrier is the OCI label now, but the file itself is
// still the build-time source of truth — see harness/DESIGN.md) and converts
// its harness contract to config.Harness, the same shape sandbox.json decodes.
func claudeImageHarness(t *testing.T) config.Harness {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "harness", "claude-code", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata discoboxharness.ImageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Harness == nil {
		t.Fatal("claude-code image.json has no harness contract")
	}
	files := make([]config.HarnessFile, 0, len(metadata.Harness.Files))
	for _, file := range metadata.Harness.Files {
		files = append(files, config.HarnessFile{
			Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly, Template: file.Template,
		})
	}
	return config.Harness{
		ID:      metadata.Harness.ID,
		Name:    metadata.Harness.Name,
		Command: metadata.Harness.RunCommand,
		Files:   files,
	}
}

func TestFileInstallerRendersSandboxConfigTemplate(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{
			"sources": []any{
				map[string]any{"slug": "primary", "target": `/workspace/project"quoted`},
				map[string]any{"slug": "extra", "target": "/workspace/extra"},
			},
		},
	}
	harness := claudeImageHarness(t)

	if err := installer.EnsureInstalled(context.Background(), harness, "", nil); err != nil {
		t.Fatalf("install files: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse rendered config: %v\n%s", err, data)
	}
	if !state.Projects[`/workspace/project"quoted`].Trusted {
		t.Fatalf("projects = %#v, want safely encoded trusted project", state.Projects)
	}
	if len(state.Projects) != 1 {
		t.Fatalf("projects = %#v, want only the primary source trusted", state.Projects)
	}
}

func TestFileInstallerDoesNotRenderLiteralFile(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{"name": "rendered"},
	}
	harness := config.Harness{ID: "literal", Files: []config.HarnessFile{{Path: "config", Content: `{{ .name }}`}}}
	if err := installer.EnsureInstalled(context.Background(), harness, "", nil); err != nil {
		t.Fatalf("install files: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{{ .name }}` {
		t.Fatalf("literal content = %q", data)
	}
}

func TestClaudeTemplateSupportsSandboxWithoutPrimarySource(t *testing.T) {
	for _, file := range claudeImageHarness(t).Files {
		if file.Path != ".claude.json" {
			continue
		}
		rendered, err := renderHarnessFileTemplate(file.Path, file.Content, map[string]any{})
		if err != nil {
			t.Fatalf("render Claude template: %v", err)
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(rendered), &state); err != nil {
			t.Fatalf("parse Claude config: %v\n%s", err, rendered)
		}
		if _, ok := state["projects"]; ok {
			t.Fatalf("source-less Claude config unexpectedly has projects: %#v", state)
		}
		return
	}
	t.Fatal("Claude definition has no .claude.json template")
}
