package harnessconfigs

import (
	"context"
	"io"
	golog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	imagetypes "github.com/moby/moby/api/types/image"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// testRegistry serves an in-process registry and returns its host.
func testRegistry(t *testing.T) string {
	t.Helper()
	handler := registry.New(registry.Logger(golog.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sandbox-agent probes every port that starts listening once; it
		// is not a registry request (repository REVIEW.md).
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return host.Host
}

// archImage is an image for one platform whose label names its architecture,
// so a test can tell which platform's labels were read.
func archImage(t *testing.T, p v1.Platform) v1.Image {
	t.Helper()
	image, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS: p.OS, Architecture: p.Architecture,
		Config: v1.Config{Labels: map[string]string{"arch": p.Architecture}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// pushIndex pushes an index of one image per platform, and resolves it the way
// the inspector does. It also returns the tag it was pushed to.
func pushIndex(t *testing.T, platforms ...v1.Platform) (*remote.Descriptor, name.Tag) {
	t.Helper()
	tag, err := name.NewTag(testRegistry(t) + "/harness:test")
	if err != nil {
		t.Fatal(err)
	}
	var index v1.ImageIndex = empty.Index
	for _, p := range platforms {
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add:        archImage(t, p),
			Descriptor: v1.Descriptor{Platform: &p},
		})
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	pool := platform.Pool()
	descriptor, err := remote.Get(tag, remote.WithPlatform(v1.Platform{OS: pool.OS, Architecture: pool.Arch}))
	if err != nil {
		t.Fatal(err)
	}
	return descriptor, tag
}

func labelArch(t *testing.T, image v1.Image) string {
	t.Helper()
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	return config.Config.Labels["arch"]
}

// A release image is published for several platforms, and its index says
// which: the harness records every one, so a pool of any of them runs it
// (ADR 0145 §1). The attestation manifests a build adds are not platforms.
// Its labels are read from the platform a pool on this machine hosts.
func TestPublishedImageReadsAnIndexsPlatforms(t *testing.T) {
	pool := platform.Pool()
	other := "riscv64"
	descriptor, _ := pushIndex(t,
		v1.Platform{OS: "linux", Architecture: pool.Arch},
		v1.Platform{OS: "linux", Architecture: other},
		v1.Platform{OS: "unknown", Architecture: "unknown"},
	)
	image, platforms, err := publishedImage(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	want := platform.NewSet(pool, platform.Platform{OS: "linux", Arch: other})
	if platforms.String() != want.String() {
		t.Fatalf("platforms = %q, want %q", platforms, want)
	}
	if got := labelArch(t, image); got != pool.Arch {
		t.Fatalf("labels read from %q, want this machine's %q", got, pool.Arch)
	}
}

// An image not published for this machine's platform still declares the same
// harness: its labels are read from a platform it is published for, and it
// records only that one.
func TestPublishedImageReadsAnotherPlatformsLabels(t *testing.T) {
	descriptor, _ := pushIndex(t, v1.Platform{OS: "linux", Architecture: "riscv64"})
	image, platforms, err := publishedImage(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if platforms.String() != "linux/riscv64" {
		t.Fatalf("platforms = %q, want linux/riscv64", platforms)
	}
	if got := labelArch(t, image); got != "riscv64" {
		t.Fatalf("labels read from %q, want riscv64", got)
	}
}

// Registration records what the image is published for, as inspection read it.
func TestCreateHarnessConfigRecordsWhatTheImagePublishes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const image = "ghcr.io/example/harness:v1"
	published := platform.NewSet(platform.Platform{OS: "linux", Arch: "amd64"}, platform.Platform{OS: "linux", Arch: "arm64"})
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		image: {Digest: "sha256:one", Platforms: published, ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{ID: "example", Name: "Example"}}},
	}}
	svc := &Service{store: st, inspector: inspector}
	config, err := svc.CreateHarnessConfig(ctx, "project-1", services.CreateHarnessConfigBody{Image: image})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if config.Platforms.String() != published.String() {
		t.Fatalf("platforms = %q, want %q", config.Platforms, published)
	}
}

// A built-in recorded before platforms were — or with a single-platform
// development image — is rewritten with what its image now publishes, even
// when its reference and digest have not moved.
func TestSeedBuiltInsRewritesABuiltInWhosePlatformsMoved(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const image = "discobox-harness-codex:v1"
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "codex", Name: "Codex", BuiltIn: true,
		Image: image, ImageDigest: "sha256:same", RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatal(err)
	}
	published := platform.NewSet(platform.Platform{OS: "linux", Arch: "amd64"}, platform.Platform{OS: "linux", Arch: "arm64"})
	inspector := &stubInspector{byImage: map[string]imageMetadata{image: {Digest: "sha256:same", Platforms: published}}}
	svc := &Service{store: st, inspector: inspector, harnessImages: map[string]string{"codex": image}}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if got.Platforms.String() != published.String() {
		t.Fatalf("platforms = %q, want %q", got.Platforms, published)
	}
}

// A multi-arch image pulled into this machine's daemon is reported there as
// the daemon's one platform; its registry, asked by the digest the daemon
// recorded, says what it is published for. A registry that cannot be asked
// answers nothing — which rules nothing out — rather than narrowing it.
func TestRegistryPlatformsOfAPulledImage(t *testing.T) {
	descriptor, tag := pushIndex(t,
		v1.Platform{OS: "linux", Architecture: "amd64"},
		v1.Platform{OS: "linux", Architecture: "arm64"},
	)
	pulled := tag.Context().Digest(descriptor.Digest.String()).String()
	if got := registryPlatforms(context.Background(), pulled); got.String() != "linux/amd64, linux/arm64" {
		t.Fatalf("platforms = %q, want both the index publishes", got)
	}
	if got := registryPlatforms(context.Background(), "127.0.0.1:1/gone@"+descriptor.Digest.String()); got != nil {
		t.Fatalf("platforms = %q from an unreachable registry, want none", got)
	}
}

// manifestOf is one entry of the manifest list a daemon reports for an image.
func manifestOf(kind imagetypes.ManifestKind, os, arch string, available bool) imagetypes.ManifestSummary {
	manifest := imagetypes.ManifestSummary{Kind: kind, Available: available}
	if kind == imagetypes.ManifestKindImage {
		manifest.ImageData = &imagetypes.ImageProperties{Platform: ocispec.Platform{OS: os, Architecture: arch}}
	}
	return manifest
}

// The daemon's manifest list says what a local image is published for. A
// pulled multi-platform image lists every platform its index has, held here or
// not; an image built here lists the one it was built for — what a development
// build makes — and attestations are not platforms. A daemon that reports no
// list answers nil, for the fallback to decide.
func TestLocalPlatformsReadTheDaemonsManifestList(t *testing.T) {
	pulled := imagetypes.InspectResponse{Manifests: []imagetypes.ManifestSummary{
		manifestOf(imagetypes.ManifestKindImage, "linux", "amd64", true),
		manifestOf(imagetypes.ManifestKindImage, "linux", "arm64", false),
		manifestOf(imagetypes.ManifestKindAttestation, "", "", true),
	}}
	if got := localPlatforms(pulled); got.String() != "linux/amd64, linux/arm64" {
		t.Fatalf("pulled image platforms = %q, want both its index lists", got)
	}
	built := imagetypes.InspectResponse{Manifests: []imagetypes.ManifestSummary{
		manifestOf(imagetypes.ManifestKindImage, "linux", "amd64", true),
		manifestOf(imagetypes.ManifestKindAttestation, "", "", true),
	}}
	if got := localPlatforms(built); got.String() != "linux/amd64" {
		t.Fatalf("built image platforms = %q, want the one it was built for", got)
	}
	if got := localPlatforms(imagetypes.InspectResponse{Os: "linux", Architecture: "amd64"}); got != nil {
		t.Fatalf("platforms = %q with no manifest list, want nil for the fallback", got)
	}
	// Without a manifest list, an image built here — no registry digest — is
	// the one platform the daemon reports.
	if got := localPlatformsWithoutManifests(context.Background(), imagetypes.InspectResponse{Os: "linux", Architecture: "arm64"}); got.String() != "linux/arm64" {
		t.Fatalf("platforms = %q, want the built image's own", got)
	}
}
