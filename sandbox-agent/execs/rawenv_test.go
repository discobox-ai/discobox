package execs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

func TestRawSecretsReachTheProcessAndNoRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "API_KEY"), []byte("the-raw-value"), 0o400); err != nil {
		t.Fatal(err)
	}
	runner := &fakeUnitManager{}
	runtimeDir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace", RuntimeDir: runtimeDir, Units: runner,
		RawSecrets: func() map[string]string {
			return map[string]string{"API_KEY": sandboxconfig.RawDeliveryEnv, "SIGNING_KEY": sandboxconfig.RawDeliveryFile}
		},
		RawSecretPath: func(name string) string { return filepath.Join(dir, name) },
	})
	if err != nil {
		t.Fatal(err)
	}
	exec, err := manager.Create(context.Background(), CreateRequest{Command: []string{"env"}})
	if err != nil {
		t.Fatal(err)
	}
	start := runner.starts[len(runner.starts)-1]
	if got := start.Env["SIGNING_KEY"]; got != filepath.Join(dir, "SIGNING_KEY") {
		t.Fatalf("file-delivered variable = %q, want its path", got)
	}
	if _, ok := start.Env["API_KEY"]; ok {
		t.Fatal("env-delivered value rode in the start request's environment")
	}
	if got := start.RawEnv["API_KEY"]; got != filepath.Join(dir, "API_KEY") {
		t.Fatalf("RawEnv = %v, want API_KEY's file", start.RawEnv)
	}
	if got := rawEnvList(start.RawEnv); !slices.Equal(got, []string{"API_KEY=the-raw-value"}) {
		t.Fatalf("process environment additions = %v", got)
	}
	argv, err := shimArgv("/agent", start)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseShimArgs(argv[2:])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Config.RawEnv["API_KEY"] != filepath.Join(dir, "API_KEY") {
		t.Fatalf("shim RawEnv = %v", parsed.Config.RawEnv)
	}
	for _, arg := range argv {
		if strings.Contains(arg, "the-raw-value") {
			t.Fatal("value on the shim's command line")
		}
	}
	got, ok := manager.Get(exec.ID)
	if !ok {
		t.Fatal("exec not found")
	}
	if _, ok := got.Env["API_KEY"]; ok {
		t.Fatal("exec record carries the env-delivered variable")
	}
	data, err := os.ReadFile(filepath.Join(runtimeDir, exec.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "the-raw-value") {
		t.Fatal("runtime record carries the value")
	}

	// A variable the caller names outright is the caller's.
	if _, err := manager.Create(context.Background(), CreateRequest{Command: []string{"env"}, Env: map[string]string{"API_KEY": "mine"}}); err != nil {
		t.Fatal(err)
	}
	if raw := runner.starts[len(runner.starts)-1].RawEnv; len(raw) != 0 {
		t.Fatalf("RawEnv = %v, want none for a variable the caller set", raw)
	}
}

func TestRawEnvListLeavesOutAValueWhoseFileIsGone(t *testing.T) {
	if got := rawEnvList(map[string]string{"GONE": filepath.Join(t.TempDir(), "GONE")}); len(got) != 0 {
		t.Fatalf("rawEnvList = %v", got)
	}
}

func TestAStartWaitsForTheRawSecretsTheSandboxWasTold(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace", RuntimeDir: t.TempDir(), Units: runner,
		AwaitRawSecrets: func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := manager.Create(context.Background(), CreateRequest{Command: []string{"env"}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Create returned before the raw secrets were in place: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if len(runner.starts) != 0 {
		t.Fatal("a process started before the raw secrets were in place")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(runner.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(runner.starts))
	}
}
