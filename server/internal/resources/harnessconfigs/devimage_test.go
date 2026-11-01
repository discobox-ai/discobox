package harnessconfigs

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
)

type recordingInspector struct {
	calls int
	err   error
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
)

// buildEntry is a build-mode bundle for target, as the watcher describes one.
func buildEntry(reference, dockerfile, target string, args map[string]string) devimage.Bundle {
	return devimage.Bundle{Reference: reference, Build: &devimage.BuildSpec{Dockerfile: dockerfile, Context: "/repo", Target: target, Args: args}}
}

// buildModeBundles is a build-mode manifest's bundles: the platform bundle,
// and codex's.
func buildModeBundles(t *testing.T, metadata string) *devbundles.Set {
	return devbundles.New([]devimage.Bundle{
		buildEntry(platformReference, "sandbox-agent/Dockerfile", platformBundleTarget, map[string]string{harness.LayerMetadataBuildArg: platformLayerJSON}),
		buildEntry(codexBundle, "harness/codex-cli/Dockerfile", harnessBundleTarget, map[string]string{harness.MetadataBuildArg: metadata}),
	}, t.TempDir())
}

// buildModeService resolves the built-ins against the build-mode bundles,
// with every other lookup refused.
func buildModeService(t *testing.T, bundles *devbundles.Set) (*Service, *recordingInspector) {
	fallback := &recordingInspector{err: errors.New("registry must not be consulted")}
	svc := &Service{store: newTestStore(t), harnesses: harnessdefs.References{PlatformBundle: platformReference}}
	svc.inspector = newDevImageInspector(bundles, fallback, nil)
	return svc, fallback
}

// In build-mode nothing ever inspects a built bundle, so each one's label has
// to be rebuilt from the manifest, and the platform's layer composed under the
// harness's — otherwise a Windows or macOS developer resolves a manifest with
// no volumes and no env.
func TestResolveComposesThePlatformLayerInBuildMode(t *testing.T) {
	svc, fallback := buildModeService(t, buildModeBundles(t, harnessMetadataJSON))

	metadata, err := svc.resolveHarness(context.Background(), svc.platformBundle(), codexBundle)
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
	// The reference is content-addressed over its inputs, so it is its own
	// freshness key; there is no digest until something builds it.
	if metadata.BundleDigest != codexBundle {
		t.Fatalf("bundle digest = %q, want the build-mode reference", metadata.BundleDigest)
	}
}

// A build-mode bundle missing its argument is a build that forgot it, as a
// built one labeled with the empty string is.
func TestResolveRefusesABuildModeBundleWithNoMetadata(t *testing.T) {
	svc, _ := buildModeService(t, buildModeBundles(t, ""))
	if _, err := svc.resolveHarness(context.Background(), svc.platformBundle(), codexBundle); err == nil {
		t.Fatal("resolveHarness() = nil, want a bundle built without its argument refused")
	}
}

// Any bundle the development set does not hold still goes to the daemon and
// registry, so production behavior is untouched.
func TestInspectFallsBackForWhatTheDevelopmentBundlesDoNotHold(t *testing.T) {
	fallback := &recordingInspector{}
	inspector := newDevImageInspector(buildModeBundles(t, harnessMetadataJSON), fallback, nil)

	if _, err := inspector.InspectBundle(context.Background(), "ghcr.io/discobox-ai/other-bundle:v1", admitAny); err != nil {
		t.Fatalf("InspectBundle() error = %v", err)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback inspector called %d times, want 1", fallback.calls)
	}
}

// A manifest with no bundles describes nothing to inspect, so wrapping must
// be a no-op rather than an inspector that shadows every lookup with a miss.
func TestNoDevelopmentBundlesLeaveTheInspectorUnchanged(t *testing.T) {
	fallback := &recordingInspector{}
	if got := newDevImageInspector(devbundles.New(nil, t.TempDir()), fallback, nil); got != imageInspector(fallback) {
		t.Fatalf("inspector = %#v, want the fallback returned unchanged", got)
	}
}
