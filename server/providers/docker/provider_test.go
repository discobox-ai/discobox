package docker

import (
	"testing"

	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

func TestEffectiveWorkerImageUsesProviderImageBeforeGlobalDefault(t *testing.T) {
	t.Setenv(dockerworker.WorkerImageEnv, "worker:global")

	if got := EffectiveWorkerImage("worker:provider"); got != "worker:provider" {
		t.Fatalf("effective worker image = %q, want provider image", got)
	}
	if got := WorkerImageSource("worker:provider"); got != "provider" {
		t.Fatalf("worker image source = %q, want provider", got)
	}
}

func TestEffectiveWorkerImageUsesGlobalWhenProviderImageMissing(t *testing.T) {
	t.Setenv(dockerworker.WorkerImageEnv, "worker:global")

	if got := EffectiveWorkerImage(""); got != "worker:global" {
		t.Fatalf("effective worker image = %q, want global image", got)
	}
	if got := WorkerImageSource(""); got != "global" {
		t.Fatalf("worker image source = %q, want global", got)
	}
}

func TestEffectiveWorkerImageUsesStaticDefaultWhenUnset(t *testing.T) {
	if got := EffectiveWorkerImage(""); got != DefaultImage() {
		t.Fatalf("effective worker image = %q, want static default", got)
	}
	if got := WorkerImageSource(""); got != "default" {
		t.Fatalf("worker image source = %q, want default", got)
	}
}
