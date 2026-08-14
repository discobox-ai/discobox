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
		{name: "pool-agent", baseImage: "discobox-pool-agent:local"},
		{name: sandboxAgentSpecName, baseImage: "discobox-sandbox-agent:local"},
		{name: "harness-codex", baseImage: "discobox-harness-codex:local", sandboxBase: true},
	}
	present := map[string]struct{}{
		"discobox-pool-agent:local":    {},
		"discobox-sandbox-agent:local": {},
		"discobox-harness-codex:local": {},
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
		{name: "pool-agent", baseImage: "discobox-pool-agent:local"},
		{name: sandboxAgentSpecName, baseImage: "discobox-sandbox-agent:local"},
		{name: "harness-codex", baseImage: "discobox-harness-codex:local", sandboxBase: true},
		{name: "harness-claude-code", baseImage: "discobox-harness-claude-code:local", sandboxBase: true},
	}
	present := map[string]struct{}{
		"discobox-pool-agent:local":          {},
		"discobox-harness-codex:local":       {},
		"discobox-harness-claude-code:local": {},
	}

	got := specNames(missingFrom(specs, present))
	want := map[string]bool{sandboxAgentSpecName: true, "harness-codex": true, "harness-claude-code": true}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want the sandbox agent and both harnesses", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected rebuild of %q", name)
		}
	}
}
