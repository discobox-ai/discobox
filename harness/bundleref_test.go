package harness

import "testing"

func TestBundleRefUsesLocalTagByDefault(t *testing.T) {
	if got := BundleRef("discobox-harness-shell"); got != "discobox-harness-shell:local" {
		t.Fatalf("BundleRef = %q, want the unqualified development tag", got)
	}
}

// A release sets both at link time. The reference has to be fully qualified, or
// Docker resolves the bare name against Docker Hub — which is how a released
// binary asked index.docker.io for library/discobox-harness-shell.
func TestBundleRefQualifiesWithRegistryAndTag(t *testing.T) {
	t.Cleanup(func(registry, tag string) func() {
		return func() { BundleRegistry, BundleTag = registry, tag }
	}(BundleRegistry, BundleTag))

	for _, registry := range []string{"ghcr.io/discobox-ai", "ghcr.io/discobox-ai/"} {
		BundleRegistry, BundleTag = registry, "v1.2.3"
		const want = "ghcr.io/discobox-ai/discobox-harness-codex:v1.2.3"
		if got := BundleRef("discobox-harness-codex"); got != want {
			t.Fatalf("BundleRef with registry %q = %q, want %q", registry, got, want)
		}
	}
}
