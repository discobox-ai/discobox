package execs

import (
	"context"
	"testing"
)

func TestExecutionsReceiveLiveSecretEnvironment(t *testing.T) {
	secrets := map[string]string{"GH_TOKEN": "sentinel-one", "FILE_TOKEN": "sentinel-file"}
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace", RuntimeDir: t.TempDir(), Units: runner,
		Env:         map[string]string{"BASE": "base", "GH_TOKEN": "base-token"},
		SecretEnv:   func() map[string]string { return secrets },
		FileSecrets: []string{"FILE_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []bool{false, true} {
		req := CreateRequest{Shell: shell}
		if !shell {
			req.Command = []string{"echo", "ok"}
		}
		if _, err := manager.Create(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		env := runner.starts[len(runner.starts)-1].Env
		if env["GH_TOKEN"] != secrets["GH_TOKEN"] || env["BASE"] != "base" {
			t.Fatalf("unexpected environment: %v", env)
		}
		if _, ok := env["FILE_TOKEN"]; ok {
			t.Fatal("file credential exported")
		}
		secrets["GH_TOKEN"] = "sentinel-two"
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Command: []string{"echo"}, Env: map[string]string{"GH_TOKEN": "override"}}); err != nil {
		t.Fatal(err)
	}
	if got := runner.starts[len(runner.starts)-1].Env["GH_TOKEN"]; got != "override" {
		t.Fatalf("override = %q", got)
	}
	if secrets["FILE_TOKEN"] != "sentinel-file" {
		t.Fatal("file template's sentinel was removed")
	}
}
