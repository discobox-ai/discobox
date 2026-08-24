package harness

import "testing"

func TestImageRefUsesLocalTagByDefault(t *testing.T) {
	if got := ImageRef("discobox-harness-shell"); got != "discobox-harness-shell:local" {
		t.Fatalf("ImageRef = %q, want the unqualified development tag", got)
	}
}

// A release sets both at link time. The reference has to be fully qualified, or
// Docker resolves the bare name against Docker Hub — which is how a released
// binary asked index.docker.io for library/discobox-harness-shell.
func TestImageRefQualifiesWithRegistryAndTag(t *testing.T) {
	t.Cleanup(func(registry, tag string) func() {
		return func() { ImageRegistry, ImageTag = registry, tag }
	}(ImageRegistry, ImageTag))

	for _, registry := range []string{"ghcr.io/discobox-ai", "ghcr.io/discobox-ai/"} {
		ImageRegistry, ImageTag = registry, "v1.2.3"
		const want = "ghcr.io/discobox-ai/discobox-harness-codex:v1.2.3"
		if got := ImageRef("discobox-harness-codex"); got != want {
			t.Fatalf("ImageRef with registry %q = %q, want %q", registry, got, want)
		}
	}
}
