package terminal

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

func TestTerminalUsesSharedSecretEnvironment(t *testing.T) {
	units := &fakeUnits{}
	manager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: t.TempDir(), RuntimeDir: t.TempDir(), Units: units,
		SecretEnv: func() map[string]string {
			return map[string]string{"GH_TOKEN": "sentinel-env", "FILE_TOKEN": "sentinel-file"}
		},
		FileSecrets: []string{"FILE_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(ServiceConfig{
		WorkingRoot: t.TempDir(), RuntimeDir: t.TempDir(), Execs: manager, Harness: config.Harness{ID: "test", Command: []string{"echo", "ok"}}, Installer: &noopInstaller{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatal(err)
	}
	env := units.starts[0].Env
	if env["GH_TOKEN"] != "sentinel-env" {
		t.Fatal("terminal did not receive shared secret environment")
	}
	if _, ok := env["FILE_TOKEN"]; ok {
		t.Fatal("file-delivered secret was exported")
	}
}
