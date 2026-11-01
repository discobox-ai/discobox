package harnessconfigs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// layerOf is one layer holding a file at each path.
func layerOf(t *testing.T, paths ...string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, path := range paths {
		if err := w.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// pushBundle pushes a single-manifest bundle with labels, one layer per path
// list, to a registry of its own, and returns its reference.
func pushBundle(t *testing.T, labels map[string]string, layers ...[]string) string {
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
	tag, err := name.NewTag(testRegistry(t) + "/bundle:test")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, image); err != nil {
		t.Fatal(err)
	}
	return tag.String()
}

// A bundle is read for its labels and pinned by the digest its registry
// serves it under.
func TestInspectBundleReadsItsLabelsAndDigest(t *testing.T) {
	labels := map[string]string{harness.ImageLabel: `{"platformAPIVersion":"discobox.ai/platform/v1"}`}
	ref := pushBundle(t, labels, []string{"features/acme/provision.yaml", "features/acme/install.sh"}, []string{"features/acme-extra/provision.yaml"})

	inspected, err := defaultImageInspector{}.InspectBundle(context.Background(), ref, admitAny)
	if err != nil {
		t.Fatalf("InspectBundle() error = %v", err)
	}
	if inspected.Labels[harness.ImageLabel] != labels[harness.ImageLabel] {
		t.Fatalf("labels = %v, want the bundle's", inspected.Labels)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := remote.Get(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Digest != descriptor.Digest.String() {
		t.Fatalf("digest = %q, want the registry's %q", inspected.Digest, descriptor.Digest)
	}
}

// A bundle's layers add feature directories side by side, so one that deletes
// what a layer beneath holds is refused, an opaque directory marker included.
func TestInspectBundleRefusesAWhiteout(t *testing.T) {
	for name, whiteout := range map[string]string{
		"a file":            "features/acme/.wh.install.sh",
		"an opaque marker":  "features/acme/.wh..wh..opq",
		"a feature removed": "features/.wh.acme",
	} {
		t.Run(name, func(t *testing.T) {
			ref := pushBundle(t, map[string]string{}, []string{"features/acme/install.sh"}, []string{whiteout})
			_, err := defaultImageInspector{}.InspectBundle(context.Background(), ref, admitAny)
			if err == nil || !strings.Contains(err.Error(), "whiteout") || !strings.Contains(err.Error(), whiteout) {
				t.Fatalf("err = %v, want the whiteout %q refused", err, whiteout)
			}
		})
	}
}

// bundleService registers against a stub whose harness bundle and platform
// bundle are separate references, as a real registration's are.
func bundleService(t *testing.T) (*Service, *stubInspector) {
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		"example.com/acme:v1": {BundleDigest: "sha256:bundle", ImageMetadata: harness.ImageMetadata{
			Env: map[string]string{"ACME": "1"}, Harness: &harness.Image{ID: "acme", Name: "Acme"},
		}},
	}}
	return &Service{store: newTestStore(t), inspector: inspector, harnesses: testReferences}, inspector
}

// A harness is its bundle (ADR 26-10-16-423 §2, ADR 26-11-01-122): it is
// registered with no image, platform or kind, and runs on whatever box a pool
// applies it to.
func TestCreateRegistersTheBundle(t *testing.T) {
	svc, _ := bundleService(t)
	config, err := svc.CreateHarnessConfig(context.Background(), "project-1", services.CreateHarnessConfigBody{
		Bundle: apigen.NewOptString("example.com/acme:v1"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if config.Slug != "acme" || config.Env["ACME"] != "1" {
		t.Fatalf("config = %+v, want the bundle's manifest", config)
	}
	if config.Bundle != "example.com/acme:v1" || config.BundleDigest != "sha256:bundle" {
		t.Fatalf("bundle = %q@%q, want the registered bundle and its digest", config.Bundle, config.BundleDigest)
	}
}

func TestCreateRefusesAHarnessWithNoBundle(t *testing.T) {
	svc, _ := bundleService(t)
	_, err := svc.CreateHarnessConfig(context.Background(), "project-1", services.CreateHarnessConfigBody{})
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "bundle is required") {
		t.Fatalf("err = %v, want a missing bundle refused as a bad request", err)
	}
}

// A harness whose platform bundle cannot be read registers nothing: its
// manifest is composed on that bundle.
func TestCreateRefusesWhenThePlatformBundleIsUnavailable(t *testing.T) {
	svc, _ := bundleService(t)
	svc.inspector = platformUnavailable{svc.inspector}
	_, err := svc.CreateHarnessConfig(context.Background(), "project-1", services.CreateHarnessConfigBody{
		Bundle: apigen.NewOptString("example.com/acme:v1"),
	})
	if err == nil || !strings.Contains(err.Error(), "platform bundle") {
		t.Fatalf("err = %v, want the platform bundle named", err)
	}
}

type platformUnavailable struct{ imageInspector }

func (p platformUnavailable) InspectBundle(ctx context.Context, ref string, admit func(map[string]string) error) (bundleInspection, error) {
	if ref == testPlatformBundle {
		return bundleInspection{}, errors.New("no such image")
	}
	return p.imageInspector.InspectBundle(ctx, ref, admit)
}

// legacyConfig is a harness registered before bundles: its manifest came from
// its image's labels, and it has no bundle, nor the image, which an upgraded
// database no longer records (internal/database).
func legacyConfig(t *testing.T, svc *Service) *model.HarnessConfig {
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "acme", Name: "Acme", RunCommand: []string{"acme"},
	}
	if err := svc.store.CreateHarnessConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	return config
}

// Its old manifest is not read again, so a refresh has nothing to read it
// from, and says how to give it one.
func TestRefreshRefusesAHarnessRegisteredBeforeBundles(t *testing.T) {
	svc, _ := bundleService(t)
	config := legacyConfig(t, svc)
	_, err := svc.RefreshHarnessConfigImage(context.Background(), "project-1", config.ID)
	if err == nil || !strings.Contains(err.Error(), "register its bundle") {
		t.Fatalf("err = %v, want the refresh refused, naming what to do", err)
	}
	got, err := svc.store.GetHarnessConfig(context.Background(), "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bundle != "" || len(got.RunCommand) != 1 || got.RunCommand[0] != "acme" {
		t.Fatalf("config = %+v, want it untouched", got)
	}
}

// Registering a bundle for it is what moves it on: the manifest is read from
// the bundle, which is what its sandboxes run.
func TestUpdateRegistersABundleForAHarnessRegisteredBeforeBundles(t *testing.T) {
	svc, _ := bundleService(t)
	config := legacyConfig(t, svc)
	body := services.UpdateHarnessConfigBody{Revision: config.Revision}
	body.SetBundle(apigen.NewOptString("example.com/acme:v1"))
	updated, err := svc.UpdateHarnessConfig(context.Background(), "project-1", config.ID, body)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Bundle != "example.com/acme:v1" || updated.BundleDigest != "sha256:bundle" || updated.Env["ACME"] != "1" {
		t.Fatalf("config = %+v, want the bundle registered and its manifest snapshotted", updated)
	}
}

// A bundle that cannot be read changes nothing.
func TestUpdateWithAnUnreadableBundleChangesNothing(t *testing.T) {
	svc, _ := bundleService(t)
	config := legacyConfig(t, svc)
	svc.inspector = platformUnavailable{svc.inspector}
	body := services.UpdateHarnessConfigBody{Revision: config.Revision}
	body.SetBundle(apigen.NewOptString("example.com/acme:v1"))
	if _, err := svc.UpdateHarnessConfig(context.Background(), "project-1", config.ID, body); err == nil {
		t.Fatal("update = nil, want an unreadable platform bundle refused")
	}
	got, err := svc.store.GetHarnessConfig(context.Background(), "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bundle != "" || got.BundleDigest != "" || len(got.RunCommand) != 1 {
		t.Fatalf("config = %+v, want it untouched", got)
	}
}

// The snapshot a bundle takes rewrites the files a person would set beside
// it, so a bundle is registered on its own, and the rest is refused rather
// than saved in a second write it overwrites.
func TestUpdateRefusesABundleBesideAnotherChange(t *testing.T) {
	svc, _ := bundleService(t)
	config := legacyConfig(t, svc)
	body := services.UpdateHarnessConfigBody{Revision: config.Revision}
	body.SetBundle(apigen.NewOptString("example.com/acme:v1"))
	body.SetName(apigen.NewOptString("Renamed"))
	if _, err := svc.UpdateHarnessConfig(context.Background(), "project-1", config.ID, body); err == nil || !strings.Contains(err.Error(), "on its own") {
		t.Fatalf("err = %v, want a bundle beside a rename refused", err)
	}
	got, err := svc.store.GetHarnessConfig(context.Background(), "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Acme" || got.Bundle != "" {
		t.Fatalf("config = %+v, want it untouched", got)
	}
}

func TestUpdateRefusesABundleForABuiltIn(t *testing.T) {
	svc, _ := bundleService(t)
	config := legacyConfig(t, svc)
	config.BuiltIn = true
	if err := svc.store.UpdateHarnessConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	body := services.UpdateHarnessConfigBody{Revision: config.Revision}
	body.SetBundle(apigen.NewOptString("example.com/acme:v1"))
	if _, err := svc.UpdateHarnessConfig(context.Background(), "project-1", config.ID, body); err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("err = %v, want a built-in's bundle left to its server", err)
	}
}

// A bundle's layers are read once per digest: the platform bundle's are the
// agent's binaries, and a seeding pass runs on every project and every start.
// The verdict is the content's, which its digest names.
func TestInspectBundleReadsALayerOncePerDigest(t *testing.T) {
	ref := pushBundle(t, map[string]string{}, []string{"features/acme/install.sh"})
	inspector := defaultImageInspector{verified: &verifiedBundles{}}
	first, err := inspector.InspectBundle(context.Background(), ref, admitAny)
	if err != nil {
		t.Fatalf("InspectBundle() error = %v", err)
	}
	if !inspector.verified.has(first.Digest) {
		t.Fatalf("digest %s not recorded as read", first.Digest)
	}
	// Another digest is read for itself, whatever was read before.
	whiteout := pushBundle(t, map[string]string{}, []string{"features/acme/.wh.install.sh"})
	refused, err := inspector.InspectBundle(context.Background(), whiteout, admitAny)
	if err == nil {
		t.Fatalf("InspectBundle() = %+v, want a whiteout refused under its own digest", refused)
	}
}

// What a bundle's labels say is judged before any layer is read: an image
// built FROM discobox-sandbox-base named as a harness's bundle is several GB of layers, and carries the
// platform layer that refuses it anyway.
func TestInspectBundleJudgesTheLabelsBeforeAnyLayer(t *testing.T) {
	ref := pushBundle(t, map[string]string{harness.ImageLayerLabelPrefix + harness.PlatformLayer: `{}`}, []string{"etc/.wh.apt"})
	refusal := errors.New("built FROM the platform")
	_, err := defaultImageInspector{}.InspectBundle(context.Background(), ref, func(map[string]string) error { return refusal })
	if !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the labels' refusal, ahead of the whiteout in its layer", err)
	}
}

// An image built FROM discobox-sandbox-base is told from a bundle by the
// reclaim label it inherits from the base, which no FROM scratch bundle
// carries: the sandbox agent image carries the platform's layer label too,
// and would be admitted as the platform bundle by that alone.
func TestInspectBundleRefusesAnImageBuiltOnTheBaseAsEitherBundle(t *testing.T) {
	labels := map[string]string{
		harness.ReclaimLabel: harness.ReclaimLabelValue,
		harness.ImageLayerLabelPrefix + harness.PlatformLayer: `{"platformAPIVersion":"discobox.ai/platform/v1"}`,
	}
	ref := pushBundle(t, labels, []string{"usr/bin/x"})
	for name, admit := range map[string]func(map[string]string) error{"platform": admitPlatform, "harness": admitAny} {
		t.Run(name, func(t *testing.T) {
			_, err := defaultImageInspector{}.InspectBundle(context.Background(), ref, admit)
			if err == nil || !strings.Contains(err.Error(), "built FROM discobox-sandbox-base") {
				t.Fatalf("err = %v, want an image built FROM the base refused as a bundle", err)
			}
		})
	}
}
