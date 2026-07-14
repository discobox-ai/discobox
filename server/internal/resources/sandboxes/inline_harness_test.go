package sandboxes

import (
	"context"
	"reflect"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func TestCreateOptionsUsesInlineHarnessConfig(t *testing.T) {
	inline := &model.InlineHarnessConfig{
		InstallCommand:  []string{"install"},
		RunCommand:      []string{"run"},
		RelaunchCommand: []string{"relaunch"},
		Files:           []model.HarnessConfigFile{{Path: "config.json", Content: "{}"}},
	}
	opts := (&SandboxReconciler{}).createOptionsFromSandbox(context.Background(), &model.Sandbox{
		ID:                  "sandbox-1",
		ProjectID:           "project-1",
		InlineHarnessConfig: inline,
	})

	if opts.ResolvedHarnessConfig == nil {
		t.Fatal("ResolvedHarnessConfig is nil")
	}
	if opts.ResolvedHarnessConfig.ID != "inline" || opts.ResolvedHarnessConfig.Name != "inline" {
		t.Fatalf("resolved identity = %q/%q, want inline/inline", opts.ResolvedHarnessConfig.ID, opts.ResolvedHarnessConfig.Name)
	}
	if !reflect.DeepEqual(opts.ResolvedHarnessConfig.RunCommand, inline.RunCommand) || !reflect.DeepEqual(opts.ResolvedHarnessConfig.Files, inline.Files) {
		t.Fatalf("resolved harness = %#v, want inline config %#v", opts.ResolvedHarnessConfig, inline)
	}
}
