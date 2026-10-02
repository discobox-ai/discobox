package harnessconfigs

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// A multi-arch image has one config digest per architecture, so inspecting it
// without naming one asks the wrong question — and go-containerregistry answers
// linux/amd64. That is how an Apple Silicon Mac pinned the amd64 digest of a
// harness image its arm64 pool would never hold. A harness is inspected for the
// platform a pool on this machine hosts, and records that platform, so the
// digest it pins and the platform its sandboxes are placed by are one answer.
func TestARegisteredHarnessIsThePoolsPlatform(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const image = "ghcr.io/example/harness:v1"
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		image: {Digest: "sha256:one", ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{ID: "example", Name: "Example"}}},
	}}
	svc := &Service{store: st, inspector: inspector}
	config, err := svc.CreateHarnessConfig(ctx, "project-1", services.CreateHarnessConfigBody{Image: image})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := platform.Pool()
	if len(inspector.platforms) != 1 || inspector.platforms[0] != want {
		t.Fatalf("inspected for %v, want %s", inspector.platforms, want)
	}
	if config.Platform != want {
		t.Fatalf("platform = %q, want %q", config.Platform, want)
	}
}

// A built-in is seeded with the platform its catalog entry declares.
func TestSeedBuiltInsRecordsTheCatalogPlatform(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const image = "discobox-harness-shell:dev"
	inspector := &stubInspector{byImage: map[string]imageMetadata{image: {Digest: "sha256:shell"}}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"shell": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "shell")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Platform != platform.Pool() {
		t.Fatalf("platform = %q, want %q", got.Platform, platform.Pool())
	}
}
