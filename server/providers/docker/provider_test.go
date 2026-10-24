package docker

import (
	"net/url"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/releasemanifest"

	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// The sandbox base image (ADR 26-10-16-423 §10) resolves from the release
// manifest first, then the provider's setting, the server's override, and the
// image this build shipped with.
func TestEffectiveSandboxBaseImagePrefersTheReleaseThenTheProviderThenTheServer(t *testing.T) {
	const serverWide = "base:server"
	if got := dockerworker.EffectiveSandboxBaseImage("base:provider", dockerworker.ServerDefaults{SandboxBaseImage: serverWide}); got != "base:provider" {
		t.Fatalf("sandbox base image = %q, want the provider image", got)
	}
	if got := dockerworker.EffectiveSandboxBaseImage(" ", dockerworker.ServerDefaults{SandboxBaseImage: serverWide}); got != serverWide {
		t.Fatalf("sandbox base image = %q, want the server override", got)
	}
	if got := dockerworker.EffectiveSandboxBaseImage("", dockerworker.ServerDefaults{}); got != dockerworker.DefaultSandboxBaseImage {
		t.Fatalf("sandbox base image = %q, want the static default", got)
	}
	release := &releasemanifest.Manifest{Images: releasemanifest.Images{SandboxBase: "example.com/base:v9"}}
	if got := dockerworker.EffectiveSandboxBaseImage("base:provider", dockerworker.ServerDefaults{SandboxBaseImage: serverWide, Release: release}); got != "example.com/base:v9" {
		t.Fatalf("sandbox base image = %q, want the release's", got)
	}
}

// The provider's sandboxBaseImage setting reaches the engine, which preloads it.
func TestSandboxBaseImageTravelsFromProviderInstanceConfig(t *testing.T) {
	cfg, err := Decode([]byte(`{"image":"pool:test","sandboxBaseImage":"base:test"}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	engineCfg := engineConfigFor(t, cfg, []string{"http://127.0.0.1:8080"}, "unix:///var/run/docker.sock")
	if engineCfg.SandboxBaseImage != "base:test" {
		t.Fatalf("SandboxBaseImage = %q, want base:test", engineCfg.SandboxBaseImage)
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

func TestSandboxIdleTimeoutTravelsFromProviderInstanceConfig(t *testing.T) {
	cfg, err := Decode([]byte(`{"image":"pool:test","sandboxIdleTimeout":"90s"}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	engineCfg := engineConfigFor(t, cfg, []string{"http://127.0.0.1:8080"}, "unix:///var/run/docker.sock")
	if engineCfg.SandboxIdleTimeout != 90*time.Second {
		t.Fatalf("SandboxIdleTimeout = %s, want 90s", engineCfg.SandboxIdleTimeout)
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

// The pool agent dials the control-plane socket by parsing this URL with wire,
// so a socket path holding a character a URL gives meaning to must reach it
// escaped: unescaped, "#" and "?" end the path early and "%" fails to parse.
func TestControlPlaneReachEscapesTheSocketPath(t *testing.T) {
	const socket = "/run/a b#c?d%e/server.sock"
	listen := (&url.URL{Scheme: "unix", Path: socket}).String()
	reach, err := resolveControlPlaneReach([]string{listen}, "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatalf("resolveControlPlaneReach: %v", err)
	}
	got, err := wire.Parse(reach.url)
	if err != nil || got.Scheme != "unix" || got.Path != socket {
		t.Fatalf("the pool reads %q as %+v, err=%v; want socket %q", reach.url, got, err, socket)
	}
	if reach.socketDir != "/run/a b#c?d%e" {
		t.Fatalf("socketDir = %q", reach.socketDir)
	}
}
