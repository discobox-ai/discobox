package execs

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// The shim's command line is world-readable in /proc and is reported as
// process telemetry (`discobox ls -o json`, the sandbox document), so no
// environment value an exec starts with may ride on it, plainly or encoded.
func TestShimCommandLineCarriesNoEnvironmentValue(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace", RuntimeDir: t.TempDir(), Units: runner,
		Env:       map[string]string{"BASE_TOKEN": "base-env-value"},                                      //nolint:gosec // G101: a marker value, not a credential.
		SecretEnv: func() map[string]string { return map[string]string{"GH_TOKEN": "ghp_sentinelvalue"} }, //nolint:gosec // G101: a sentinel-shaped marker, not a credential.
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"env"},
		Env:     map[string]string{"DEPLOY_TOKEN": "caller-secret-value"},
	}); err != nil {
		t.Fatal(err)
	}
	start := runner.starts[len(runner.starts)-1]
	argv, err := shimArgv("/agent", start)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range argv {
		text := arg
		if decoded, err := base64.StdEncoding.DecodeString(arg); err == nil {
			text = string(decoded)
		}
		for _, value := range []string{"base-env-value", "ghp_sentinelvalue", "caller-secret-value"} {
			if strings.Contains(text, value) {
				t.Errorf("shim command line carries env value %q in %q", value, text)
			}
		}
	}
}
