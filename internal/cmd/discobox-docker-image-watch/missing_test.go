package main

import (
	"testing"
)

// A built image can leave the daemon without any file changing — reclamation
// removes a superseded one, a developer prunes — and nothing rebuilt it, so the
// manifest kept naming an image that could not come back and every pool
// reconcile failed against it.
func TestMissingImageSpecsRebuildsWhatLeftTheDaemon(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
		{name: sandboxAgentSpecName, baseImage: "discobox-sandbox-agent:local", parent: baseSpecName},
		{name: "harness-codex-image", baseImage: "discobox-harness-codex-image:local", parent: sandboxAgentSpecName},
	}
	present := map[string]struct{}{
		"discobox-sandbox-base:local":        {},
		"discobox-pool-agent:local":          {},
		"discobox-sandbox-agent:local":       {},
		"discobox-harness-codex-image:local": {},
	}

	if got := specNames(missingFrom(specs, present)); len(got) != 0 {
		t.Fatalf("missing = %v, want none when every image is present", got)
	}

	delete(present, "discobox-pool-agent:local")
	got := specNames(missingFrom(specs, present))
	if len(got) != 1 || got[0] != "pool-agent" {
		t.Fatalf("missing = %v, want [pool-agent]", got)
	}
}

// A rebuilt sandbox agent is a new base, so everything layered on it has to be
// rebuilt too — publishing a harness image built on a base that no longer exists
// is how the manifest ends up describing something unbuildable.
func TestMissingSandboxAgentRebuildsEveryImageLayeredOnIt(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
		{name: sandboxAgentSpecName, baseImage: "discobox-sandbox-agent:local", parent: baseSpecName},
		{name: "harness-codex-image", baseImage: "discobox-harness-codex-image:local", parent: sandboxAgentSpecName},
		{name: "harness-claude-code-image", baseImage: "discobox-harness-claude-code-image:local", parent: sandboxAgentSpecName},
	}
	present := map[string]struct{}{
		"discobox-sandbox-base:local":              {},
		"discobox-pool-agent:local":                {},
		"discobox-harness-codex-image:local":       {},
		"discobox-harness-claude-code-image:local": {},
	}

	got := specNames(missingFrom(specs, present))
	want := map[string]bool{sandboxAgentSpecName: true, "harness-codex-image": true, "harness-claude-code-image": true}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want the sandbox agent and both harnesses", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected rebuild of %q", name)
		}
	}
}

// The cascade is two levels deep because both agent images build FROM a shared
// base: losing it has to take the sandbox agent AND every harness on top of it,
// which a single non-repeated sweep would miss.
func TestMissingSharedBaseRebuildsEveryImageBuiltOnIt(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
		{name: sandboxAgentSpecName, baseImage: "discobox-sandbox-agent:local", parent: baseSpecName},
		{name: "harness-codex-image", baseImage: "discobox-harness-codex-image:local", parent: sandboxAgentSpecName},
	}
	present := map[string]struct{}{
		"discobox-pool-agent:local":          {},
		"discobox-sandbox-agent:local":       {},
		"discobox-harness-codex-image:local": {},
	}

	got := specNames(missingFrom(specs, present))
	if len(got) != len(specs) {
		t.Fatalf("missing = %v, want every image", got)
	}
}
