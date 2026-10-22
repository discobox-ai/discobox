package harnessconfigs

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
)

type recordingInspector struct {
	calls int
	err   error
}

func (s *recordingInspector) Inspect(context.Context, string) (imageMetadata, error) {
	s.calls++
	return imageMetadata{}, s.err
}

func (s *recordingInspector) InspectBundle(context.Context, string, func(map[string]string) error) (bundleInspection, error) {
	s.calls++
	return bundleInspection{}, s.err
}

const harnessMetadataJSON = `{"platformAPIVersion":"discobox.ai/platform/v1","harness":{"id":"codex","name":"Codex","runCommand":["codex"]}}`

// The layer the platform bundle carries, composed under every harness's.
const platformLayerJSON = `{"platformAPIVersion":"discobox.ai/platform/v1","env":{"DISPLAY":":0"},"volumes":[{"path":"%HOME%","volume":"data"}]}`

const (
	platformReference = "discobox-platform:dev-base123"
	codexBundle       = "discobox-harness-codex:dev-abc123"
	codexImage        = "discobox-harness-codex-image:dev-abc123"
)

// buildEntry is a build-mode manifest entry for target, as the watcher writes
// one.
func buildEntry(reference, dockerfile, target string, args map[string]string) devimage.Image {
	return devimage.Image{Reference: reference, Build: &devimage.BuildSpec{Dockerfile: dockerfile, Context: "/repo", Target: target, Args: args}}
}

// buildModeImages is a build-mode manifest: the platform bundle, and codex's
// bundle beside the runnable image built from it, which builds the bundle
// stage too and so carries the same argument.
func buildModeImages(metadata string) []devimage.Image {
	return []devimage.Image{
		buildEntry(platformReference, "sandbox-agent/Dockerfile", platformBundleTarget, map[string]string{harness.LayerMetadataBuildArg: platformLayerJSON}),
		buildEntry(codexBundle, "harness/codex-cli/Dockerfile", harnessBundleTarget, map[string]string{harness.MetadataBuildArg: metadata}),
		buildEntry(codexImage, "harness/codex-cli/Dockerfile", "image", map[string]string{harness.MetadataBuildArg: metadata, "SANDBOX_AGENT_IMAGE": "discobox-sandbox-agent:dev-base123"}),
	}
}

// buildModeService resolves the built-ins against the build-mode manifest,
// with every other lookup refused.
func buildModeService(t *testing.T, images []devimage.Image) (*Service, *recordingInspector) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	svc := &Service{store: newTestStore(t), harnesses: harnessdefs.References{PlatformBundle: platformReference}}
	svc.inspector = newDevImageInspector(images, nil, fallback, nil)
	return svc, fallback
}

// In build-mode nothing ever inspects a built bundle, so each one's label has
// to be rebuilt from the manifest, and the platform's layer composed under the
// harness's — otherwise a Windows or macOS developer resolves a manifest with
// no volumes and no env.
func TestResolveComposesThePlatformLayerInBuildMode(t *testing.T) {
	svc, fallback := buildModeService(t, buildModeImages(harnessMetadataJSON))

	metadata, err := svc.resolveHarness(context.Background(), svc.platformBundle(), codexBundle, codexImage)
	if err != nil {
		t.Fatalf("resolveHarness() error = %v", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback inspector was consulted %d times, want 0", fallback.calls)
	}
	if metadata.Env["DISPLAY"] != ":0" || len(metadata.Volumes) != 1 {
		t.Fatalf("metadata = %#v, want the platform layer merged under the harness's own", metadata.ImageMetadata)
	}
	if metadata.Harness == nil || metadata.Harness.ID != "codex" {
		t.Fatalf("metadata = %#v, want the harness the manifest describes", metadata)
	}
	// Each reference is content-addressed over its inputs, so it is its own
	// freshness key; there is no digest until something builds it.
	if metadata.Digest != codexImage || metadata.BundleDigest != codexBundle {
		t.Fatalf("digests = %q, %q, want the build-mode references", metadata.Digest, metadata.BundleDigest)
	}
}

// A build-mode bundle missing its argument is a build that forgot it, as a
// built one labeled with the empty string is.
func TestResolveRefusesABuildModeBundleWithNoMetadata(t *testing.T) {
	svc, _ := buildModeService(t, buildModeImages(""))
	if _, err := svc.resolveHarness(context.Background(), svc.platformBundle(), codexBundle, codexImage); err == nil {
		t.Fatal("resolveHarness() = nil, want a bundle built without its argument refused")
	}
}

// Any reference the manifest does not describe still goes to the daemon and
// registry, so copy-mode and production behavior are untouched.
func TestInspectFallsBackForImagesTheManifestDoesNotDescribe(t *testing.T) {
	fallback := &recordingInspector{}
	inspector := newDevImageInspector(buildModeImages(harnessMetadataJSON), nil, fallback, nil)

	if _, err := inspector.Inspect(context.Background(), "ghcr.io/discobox-ai/other:v1"); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if _, err := inspector.InspectBundle(context.Background(), "ghcr.io/discobox-ai/other-bundle:v1", admitAny); err != nil {
		t.Fatalf("InspectBundle() error = %v", err)
	}
	if fallback.calls != 2 {
		t.Fatalf("fallback inspector called %d times, want 2", fallback.calls)
	}
}

// A manifest with neither build descriptions nor bundles describes nothing
// to inspect, so wrapping must be a no-op rather than an inspector that
// shadows every lookup with a miss.
func TestAManifestOfImagesAloneLeavesTheInspectorUnchanged(t *testing.T) {
	fallback := &recordingInspector{}
	images := []devimage.Image{{Reference: "discobox-pool-agent:dev-abc", ID: "sha256:abc"}}
	if got := newDevImageInspector(images, nil, fallback, nil); got != imageInspector(fallback) {
		t.Fatalf("inspector = %#v, want the fallback returned unchanged", got)
	}
}
