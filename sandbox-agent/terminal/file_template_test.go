package terminal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

func claudeImageHarness(t *testing.T) config.Harness {
	t.Helper()
	image, err := config.LoadImage(filepath.Join("..", "image", "harnesses", "claude-code", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	harness, ok, err := image.HarnessForMode("run")
	if err != nil || !ok {
		t.Fatalf("load Claude image harness: ok=%v err=%v", ok, err)
	}
	return harness
}

func TestFileInstallerRendersSandboxConfigTemplate(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{
			"source": map[string]any{
				"destination": map[string]any{"directory": `/workspace/project"quoted`},
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
