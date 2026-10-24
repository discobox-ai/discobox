package dockerworker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// Unset, the idle timeout must serialize away entirely, like every pool policy
// field: configRevision hashes the config, and materializing a default would
// recreate every running pool at upgrade for a policy nobody set.
func TestUnsetSandboxIdleTimeoutLeavesPoolConfigurationUnchanged(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolPolicyEnv()
	if value, ok := env[sandboxruntime.EnvSandboxIdleTimeout]; ok {
		t.Fatalf("%s = %q, want absent when unset", sandboxruntime.EnvSandboxIdleTimeout, value)
	}
	data, err := json.Marshal(engine.cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(data), "sandboxIdleTimeout") {
		t.Fatalf("unset sandbox idle timeout serialized into the pool configuration: %s", data)
	}
}

// The pool agent writes every sandbox.json, so the timeout has to reach it to
// govern any sandbox (ADR 0108 §3).
func TestConfiguredSandboxIdleTimeoutReachesThePoolAgent(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "pool:test", SandboxIdleTimeout: 2 * time.Minute}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolPolicyEnv()
	if got := env[sandboxruntime.EnvSandboxIdleTimeout]; got != "2m0s" {
		t.Fatalf("%s = %q, want 2m0s", sandboxruntime.EnvSandboxIdleTimeout, got)
	}
}
