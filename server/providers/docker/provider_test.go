package docker

import (
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

func TestEffectivePoolImageUsesProviderImageBeforeGlobalDefault(t *testing.T) {
	t.Setenv(dockerworker.PoolImageEnv, "worker:global")

	if got := EffectivePoolImage("worker:provider"); got != "worker:provider" {
		t.Fatalf("effective worker image = %q, want provider image", got)
	}
	if got := PoolImageSource("worker:provider"); got != "provider" {
		t.Fatalf("worker image source = %q, want provider", got)
	}
}

func TestEffectivePoolImageUsesGlobalWhenProviderImageMissing(t *testing.T) {
	t.Setenv(dockerworker.PoolImageEnv, "worker:global")

	if got := EffectivePoolImage(""); got != "worker:global" {
		t.Fatalf("effective worker image = %q, want global image", got)
	}
	if got := PoolImageSource(""); got != "global" {
		t.Fatalf("worker image source = %q, want global", got)
	}
}

func TestEffectivePoolImageUsesStaticDefaultWhenUnset(t *testing.T) {
	if got := EffectivePoolImage(""); got != DefaultImage() {
		t.Fatalf("effective worker image = %q, want static default", got)
	}
	if got := PoolImageSource(""); got != "default" {
		t.Fatalf("worker image source = %q, want default", got)
	}
}

// The whole path a provider-instance setting travels: stored JSON on the
// provider instance, decoded into the provider's own Config through the
// embedded pool policy, and mapped onto the engine that renders the pool
// container's environment.
func TestProxyAuditRetentionTravelsFromProviderInstanceConfig(t *testing.T) {
	cfg, err := Decode([]byte(`{"image":"pool:test","proxyAuditRetention":"96h"}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	engineCfg := engineConfigFor(t, cfg, []string{"http://127.0.0.1:8080"}, "unix:///var/run/docker.sock")
	if engineCfg.ProxyAuditRetention != 96*time.Hour {
		t.Fatalf("ProxyAuditRetention = %s, want 96h", engineCfg.ProxyAuditRetention)
	}
}

func TestUnsetProxyAuditRetentionReachesTheEngineAsZero(t *testing.T) {
	cfg, err := Decode([]byte(`{"image":"pool:test"}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	engineCfg := engineConfigFor(t, cfg, []string{"http://127.0.0.1:8080"}, "unix:///var/run/docker.sock")
	if engineCfg.ProxyAuditRetention != 0 {
		t.Fatalf("ProxyAuditRetention = %s, want zero so the pool proxy default applies", engineCfg.ProxyAuditRetention)
	}
}

// The catalog has to offer the field, or nothing can be configured through the
// UI that reads it.
func TestDefinitionOffersTheSharedPoolPolicyFields(t *testing.T) {
	var found bool
	for _, field := range Definition().ConfigFields {
		if field.Key == "proxyAuditRetention" {
			found = true
		}
	}
	if !found {
		t.Fatal("Definition() does not offer proxyAuditRetention")
	}
}
