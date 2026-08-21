package harnessconfigs

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/devimage"
)

type recordingInspector struct {
	calls int
	err   error
}

func (s *recordingInspector) Inspect(context.Context, string) (imageMetadata, error) {
	s.calls++
	return imageMetadata{}, s.err
}

const harnessMetadataJSON = `{"harness":{"id":"codex","name":"Codex","runCommand":["codex"]}}`

// In build-mode a development image exists only as a build description: the
// host has no daemon that holds it and it was never pushed. Seeding must still
// resolve its harness metadata, or the project starts with no harness at all.
func TestInspectResolvesBuildModeMetadataWithoutDaemonOrRegistry(t *testing.T) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	inspector := newDevImageInspector([]devimage.Image{{
		Reference: "discobox-harness-codex:dev-abc123",
		Build: &devimage.BuildSpec{
			Dockerfile: "Dockerfile",
			Context:    "/repo/harness/codex-cli",
			Args:       map[string]string{harnessMetadataBuildArg: harnessMetadataJSON},
		},
	}}, fallback)

	metadata, err := inspector.Inspect(context.Background(), "discobox-harness-codex:dev-abc123")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback inspector was consulted %d times, want 0", fallback.calls)
	}
	if metadata.Harness == nil || metadata.Harness.ID != "codex" {
		t.Fatalf("metadata = %#v, want the harness described by the manifest", metadata)
	}
	// The reference is content-addressed over the image's inputs, so it is its
	// own freshness key; there is no digest until something builds it.
	if metadata.Digest != "discobox-harness-codex:dev-abc123" {
		t.Fatalf("digest = %q, want the build-mode reference", metadata.Digest)
	}
}

// Any reference the manifest does not describe still goes to the daemon and
// registry, so copy-mode and production behavior are untouched.
func TestInspectFallsBackForImagesTheManifestDoesNotDescribe(t *testing.T) {
	fallback := &recordingInspector{}
	inspector := newDevImageInspector([]devimage.Image{{
		Reference: "discobox-harness-codex:dev-abc123",
		Build: &devimage.BuildSpec{
			Dockerfile: "Dockerfile",
			Context:    "/repo/harness/codex-cli",
			Args:       map[string]string{harnessMetadataBuildArg: harnessMetadataJSON},
		},
	}}, fallback)

	if _, err := inspector.Inspect(context.Background(), "ghcr.io/discobox-ai/other:v1"); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback inspector called %d times, want 1", fallback.calls)
	}
}

// Copy-mode manifests carry no build descriptions, so wrapping must be a no-op
// rather than an inspector that shadows every lookup with a miss.
func TestCopyModeManifestLeavesTheInspectorUnchanged(t *testing.T) {
	fallback := &recordingInspector{}
	images := []devimage.Image{{Reference: "discobox-pool-agent:dev-abc", ID: "sha256:abc"}}
	if got := newDevImageInspector(images, fallback); got != imageInspector(fallback) {
		t.Fatalf("inspector = %#v, want the fallback returned unchanged", got)
	}
}
