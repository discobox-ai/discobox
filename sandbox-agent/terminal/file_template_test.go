package terminal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	claudecode "github.com/obot-platform/discobox/harness/claude-code"
	"github.com/obot-platform/discobox/sandbox-agent/config"
)

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
	agent := config.Agent{ID: "claude-code"}
	for _, file := range (claudecode.Driver{}).Definition().Files {
		agent.Files = append(agent.Files, config.AgentFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: file.CreateOnly,
			Template:   file.Template,
		})
	}

	if err := installer.EnsureInstalled(context.Background(), agent, "", nil); err != nil {
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
	agent := config.Agent{ID: "literal", Files: []config.AgentFile{{Path: "config", Content: `{{ .name }}`}}}
	if err := installer.EnsureInstalled(context.Background(), agent, "", nil); err != nil {
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
	for _, file := range (claudecode.Driver{}).Definition().Files {
		if file.Path != ".claude.json" {
			continue
		}
		rendered, err := renderAgentFileTemplate(file.Path, file.Content, map[string]any{})
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
