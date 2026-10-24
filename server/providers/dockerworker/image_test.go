package dockerworker

import (
	"testing"

	"github.com/discobox-ai/discobox/pkg/releasemanifest"
)

// The pool bundle is resolved as the sandbox base image is (ADR 26-10-24-953
// §2), but for a provider's setting: a selected release decides, then the
// server's setting, and the compiled-in default.
func TestEffectivePoolAgentBundle(t *testing.T) {
	release := &releasemanifest.Manifest{Bundles: releasemanifest.Bundles{PoolAgent: "registry.example/discobox-pool-agent-bundle:v1.0.0"}}
	for _, tc := range []struct {
		name     string
		defaults ServerDefaults
		want     string
	}{
		{name: "default", want: DefaultPoolAgentBundle},
		{name: "server", defaults: ServerDefaults{PoolAgentBundle: "server:dev-1"}, want: "server:dev-1"},
		{name: "release over server", defaults: ServerDefaults{PoolAgentBundle: "server:dev-1", Release: release}, want: release.Bundles.PoolAgent},
		{name: "a release naming none", defaults: ServerDefaults{Release: &releasemanifest.Manifest{}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectivePoolAgentBundle(tc.defaults); got != tc.want {
				t.Fatalf("EffectivePoolAgentBundle = %q, want %q", got, tc.want)
			}
		})
	}
}
