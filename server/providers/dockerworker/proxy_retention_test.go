package dockerworker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// The same rule ImageRetention lives by: an unset override must serialize away
// entirely, or configRevision changes and every pool already running is
// recreated at upgrade for a policy nobody asked for.
func TestUnsetProxyAuditRetentionLeavesPoolConfigurationUnchanged(t *testing.T) {
	engine, err := New(Config{Image: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolContainerEnv(poolagent.Bootstrap{ControlPlaneURL: "http://cp", PoolID: "pool-1", Token: "t"})
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
	engine, err := New(Config{Image: "pool:test", ProxyAuditRetention: 72 * time.Hour}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	env := engine.poolContainerEnv(poolagent.Bootstrap{ControlPlaneURL: "http://cp", PoolID: "pool-1", Token: "t"})
	if got := env[proxyagent.EnvAuditRetention]; got != "72h0m0s" {
		t.Fatalf("%s = %q, want 72h0m0s", proxyagent.EnvAuditRetention, got)
	}
}
