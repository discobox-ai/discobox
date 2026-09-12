package pools

import (
	"slices"
	"testing"
)

// The set is what this server will actually run. Reading the compiled-in
// default instead meant a build pointed at released images staged a local tag,
// skipped it for being local, and left the largest image in the set unpulled.
func TestImageSetUsesTheResolvedSandboxImage(t *testing.T) {
	previous := defaultSandboxImage()
	t.Cleanup(func() { setDefaultSandboxImage(previous) })
	setDefaultSandboxImage("ghcr.io/discobox-ai/discobox-sandbox-agent:v1.2.3")

	images := stageableImages([]string{defaultSandboxImage(), "ghcr.io/x/harness-shell:v1"})
	if !slices.Contains(images, "ghcr.io/discobox-ai/discobox-sandbox-agent:v1.2.3") {
		t.Fatalf("images = %v, want the resolved sandbox image", images)
	}
}

// A local tag exists on no registry: pulling one fails on every development
// build, where the image is already on the daemon anyway.
func TestStageableImagesSkipsLocalTags(t *testing.T) {
	images := stageableImages([]string{"discobox-sandbox-agent:local", "discobox-harness-shell:local", "ghcr.io/x/codex:v1"})
	if len(images) != 1 || images[0] != "ghcr.io/x/codex:v1" {
		t.Fatalf("images = %v, want only the registry image", images)
	}
}

// Harness configs commonly share an image, and the order must not depend on
// map iteration or a status line says something different each pass.
func TestStageableImagesDedupesAndOrders(t *testing.T) {
	images := stageableImages([]string{"ghcr.io/x/b:v1", "ghcr.io/x/a:v1", "ghcr.io/x/b:v1", "  ", ""})
	if !slices.Equal(images, []string{"ghcr.io/x/a:v1", "ghcr.io/x/b:v1"}) {
		t.Fatalf("images = %v", images)
	}
}
