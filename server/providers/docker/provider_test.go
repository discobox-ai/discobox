package docker

import (
	"testing"

	"github.com/obot-platform/discobox/server/providers/dockerworker"
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
