package dockerworker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// The same rule ImageRetention lives by: an unset override must serialize away
// entirely, or configRevision changes and every pool already running is
// recreated at upgrade for a policy nobody asked for.
func TestUnsetProxyAuditRetentionLeavesPoolConfigurationUnchanged(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolPolicyEnv()
	if value, ok := env[proxyagent.EnvAuditRetention]; ok {
		t.Fatalf("%s = %q, want absent when unset", proxyagent.EnvAuditRetention, value)
	}
	data, err := json.Marshal(engine.cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(data), "proxyAuditRetention") {
		t.Fatalf("unset proxy audit retention serialized into the pool configuration: %s", data)
	}
}

// The proxy that keeps the audit trail runs inside the pool container, so the
// window has to travel there to govern anything.
func TestConfiguredProxyAuditRetentionReachesThePoolProxy(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "pool:test", ProxyAuditRetention: 72 * time.Hour}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolPolicyEnv()
	if got := env[proxyagent.EnvAuditRetention]; got != "72h0m0s" {
		t.Fatalf("%s = %q, want 72h0m0s", proxyagent.EnvAuditRetention, got)
	}
}

// The spool budget follows the same rule: unset serializes away, and what is
// set reaches the pool proxy as written.
func TestProxyAuditSpoolBudgetReachesThePoolProxyOnlyWhenSet(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolPolicyEnv()
	for _, name := range []string{proxyagent.EnvAuditMaxSize, proxyagent.EnvAuditMaxPercent, proxyagent.EnvAuditBodyHead} {
		if value, ok := env[name]; ok {
			t.Fatalf("%s = %q, want absent when unset", name, value)
		}
	}
	data, err := json.Marshal(engine.cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(data), "proxyAudit") {
		t.Fatalf("unset proxy audit spool budget serialized into the pool configuration: %s", data)
	}

	engine, err = New(Config{SandboxBaseImage: "pool:test", AuditSpoolBudget: proxyagent.AuditSpoolBudget{MaxSize: "0", MaxPercent: "10%"}}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env = engine.poolPolicyEnv()
	if env[proxyagent.EnvAuditMaxSize] != "0" || env[proxyagent.EnvAuditMaxPercent] != "10%" {
		t.Fatalf("env = %v, want the budget passed through as written", env)
	}
	if _, ok := env[proxyagent.EnvAuditBodyHead]; ok {
		t.Fatalf("%s set though the head was not", proxyagent.EnvAuditBodyHead)
	}
}
