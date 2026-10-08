package harnessconfigs

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
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

// The layer the sandbox agent base contributes to everything built FROM it.
const baseLayerJSON = `{"env":{"DISPLAY":":0"},"volumes":[{"path":"%HOME%","volume":"data"}]}`

const sandboxAgentReference = "discobox-sandbox-agent:dev-base123"

// baseImage is the manifest entry a harness entry inherits its layer from.
func baseImage() devimage.Image {
	return devimage.Image{
		Reference: sandboxAgentReference,
		Build: &devimage.BuildSpec{
			Dockerfile: "sandbox-agent/Dockerfile",
			Context:    "/repo",
			Args:       map[string]string{harness.LayerMetadataBuildArg: baseLayerJSON},
		},
	}
}

// harnessImage is a harness entry naming the base entry the way the watcher
// writes it: the reference in SANDBOX_AGENT_IMAGE, which is also what orders
// the builds.
func harnessImage(reference, metadata string) devimage.Image {
	args := map[string]string{sandboxAgentImageBuildArg: sandboxAgentReference}
	if metadata != "" {
		args[harness.MetadataBuildArg] = metadata
	}
	return devimage.Image{
		Reference: reference,
		Build: &devimage.BuildSpec{
			Dockerfile: "Dockerfile",
			Context:    "/repo/harness/codex-cli",
			Args:       args,
		},
	}
}

// In build-mode nothing ever inspects a built image, so the inherited base
// layer has to be reassembled from the manifest — otherwise a Windows or macOS
// developer resolves a manifest with no volumes and no env, and every harness
// image is rejected as not built from the base (ADR 0086 §4).
func TestInspectInheritsTheBaseLayerInBuildMode(t *testing.T) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	inspector := newDevImageInspector([]devimage.Image{
		baseImage(),
		harnessImage("discobox-harness-codex:dev-abc123", harnessMetadataJSON),
	}, fallback)

	metadata, err := inspector.Inspect(context.Background(), "discobox-harness-codex:dev-abc123")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if metadata.Env["DISPLAY"] != ":0" || len(metadata.Volumes) != 1 {
		t.Fatalf("metadata = %#v, want the base layer merged under the harness's own", metadata.ImageMetadata)
	}
	if metadata.Harness == nil || metadata.Harness.ID != "codex" {
		t.Fatalf("metadata = %#v, want the harness the manifest describes", metadata)
	}
}

// A harness image that declares nothing is its inherited layer, and still
// registers: `shell` passes no metadata argument at all.
func TestInspectResolvesAnImageThatDeclaresNothing(t *testing.T) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	inspector := newDevImageInspector([]devimage.Image{
		baseImage(),
		harnessImage("discobox-harness-shell:dev-abc123", ""),
	}, fallback)

	metadata, err := inspector.Inspect(context.Background(), "discobox-harness-shell:dev-abc123")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if metadata.Harness != nil {
		t.Fatalf("harness = %#v, want none declared", metadata.Harness)
	}
	if metadata.Env["DISPLAY"] != ":0" {
		t.Fatalf("env = %v, want the inherited base layer's", metadata.Env)
	}
}

// In build-mode a development image exists only as a build description: the
// host has no daemon that holds it and it was never pushed. Seeding must still
// resolve its harness metadata, or the project starts with no harness at all.
func TestInspectResolvesBuildModeMetadataWithoutDaemonOrRegistry(t *testing.T) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	inspector := newDevImageInspector([]devimage.Image{
		baseImage(),
		harnessImage("discobox-harness-codex:dev-abc123", harnessMetadataJSON),
	}, fallback)

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
	inspector := newDevImageInspector([]devimage.Image{
		baseImage(),
		harnessImage("discobox-harness-codex:dev-abc123", harnessMetadataJSON),
	}, fallback)

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
