package harnessconfigs

import (
	"archive/tar"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// exportBundle writes a single-manifest bundle with labels, one layer per path
// list, as the OCI image layout tar the image watcher exports, and returns it
// as the development manifest records it.
func exportBundle(t *testing.T, reference string, labels map[string]string, layers ...[]string) devimage.Bundle {
	t.Helper()
	image, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Labels: labels}})
	if err != nil {
		t.Fatal(err)
	}
	for _, paths := range layers {
		if image, err = mutate.AppendLayers(image, layerOf(t, paths...)); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	path, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := path.AppendImage(image); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "bundle.tar")
	tarDir(t, dir, tarPath)
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return devimage.Bundle{Reference: reference, Digest: digest.String(), Layout: tarPath}
}

func tarDir(t *testing.T, dir, tarPath string) {
	t.Helper()
	file, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out := tar.NewWriter(file)
	if err := out.AddFS(os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// layoutService resolves the built-ins against bundles exported as layouts,
// with every other lookup refused.
func layoutService(t *testing.T, bundles ...devimage.Bundle) (*Service, *recordingInspector) {
	t.Helper()
	fallback := &recordingInspector{}
	svc := &Service{store: newTestStore(t), harnesses: harnessdefs.References{PlatformBundle: platformReference}}
	svc.inspector = newDevImageInspector(devbundles.New(bundles, t.TempDir()), noBundles{fallback}, &verifiedBundles{})
	return svc, fallback
}

// noBundles answers a manifest file and refuses any bundle, which on a
// development server is in no daemon or registry.
type noBundles struct{ files *recordingInspector }

func (n noBundles) InspectManifestFile(ctx context.Context, ref string) (imageMetadata, error) {
	return n.files.InspectManifestFile(ctx, ref)
}

func (noBundles) InspectBundle(context.Context, string, func(map[string]string) error) (bundleInspection, error) {
	return bundleInspection{}, errors.New("a development bundle must not be looked for in a daemon or a registry")
}

// In copy-mode the watcher exports each bundle as a layout no daemon holds:
// the harness's labels, composed on the platform's, are read from there, and
// each is pinned by the manifest its layout's index lists.
func TestResolveReadsBundlesFromTheirLayouts(t *testing.T) {
	platformBundle := exportBundle(t, platformReference,
		map[string]string{harness.ImageLayerLabelPrefix + harness.PlatformLayer: platformLayerJSON}, []string{"features/core/provision.yaml"})
	codex := exportBundle(t, codexBundle, map[string]string{harness.ImageLabel: harnessMetadataJSON}, []string{"features/codex/provision.yaml"})
	svc, fallback := layoutService(t, platformBundle, codex)

	metadata, err := svc.resolveHarness(context.Background(), svc.platformBundle(), codexBundle, "")
	if err != nil {
		t.Fatalf("resolveHarness() error = %v", err)
	}
	if fallback.calls != 0 {
		t.Fatalf("fallback inspector was consulted %d times, want 0", fallback.calls)
	}
	if metadata.Env["DISPLAY"] != ":0" || len(metadata.Volumes) != 1 || metadata.Harness == nil || metadata.Harness.ID != "codex" {
		t.Fatalf("metadata = %#v, want codex's manifest composed on the platform layer", metadata.ImageMetadata)
	}
	if metadata.BundleDigest != codex.Digest {
		t.Fatalf("bundle digest = %q, want the layout's %q", metadata.BundleDigest, codex.Digest)
	}
}

// A layout's bundle is read as any other is: one deleting what a layer
// beneath holds is refused.
func TestInspectLayoutBundleRefusesAWhiteout(t *testing.T) {
	bundle := exportBundle(t, codexBundle, map[string]string{}, []string{"features/acme/install.sh"}, []string{"features/acme/.wh.install.sh"})
	_, err := inspectLayoutBundle(bundle, admitAny, &verifiedBundles{})
	if err == nil || !strings.Contains(err.Error(), "whiteout") {
		t.Fatalf("err = %v, want the whiteout refused", err)
	}
}

// A layout the watcher has rebuilt since its digest was recorded holds
// another bundle, which nothing pins yet.
func TestInspectLayoutBundleRefusesALayoutRebuiltSince(t *testing.T) {
	bundle := exportBundle(t, codexBundle, map[string]string{harness.ImageLabel: harnessMetadataJSON}, []string{"features/codex/provision.yaml"})
	rebuilt := exportBundle(t, codexBundle, map[string]string{harness.ImageLabel: harnessMetadataJSON}, []string{"features/codex/launch.sh"})
	if err := os.Rename(rebuilt.Layout, bundle.Layout); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLayoutBundle(bundle, admitAny, &verifiedBundles{}); err == nil || !strings.Contains(err.Error(), "not the") {
		t.Fatalf("err = %v, want the rebuilt layout refused", err)
	}
	if err := os.Remove(bundle.Layout); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectLayoutBundle(bundle, admitAny, &verifiedBundles{}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want a missing layout reported as one", err)
	}
}
