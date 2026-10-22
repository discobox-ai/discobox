package harnessconfigs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/discobox-ai/discobox/devimage"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// maxLayoutDocument bounds a manifest or config read whole from a bundle's
// layout.
const maxLayoutDocument = 4 << 20

// inspectLayoutBundle reads a bundle the image watcher exported as an OCI
// image layout (devimage.Bundle): pinned by the manifest its index lists,
// labeled by that manifest's config, and its layers read for a whiteout once
// per digest, as any bundle's are.
//
// A layout whose index lists another manifest than the one the development
// manifest recorded is refused rather than pinned to content nothing recorded.
func inspectLayoutBundle(bundle devimage.Bundle, admit func(labels map[string]string) error, verified *verifiedBundles) (bundleInspection, error) {
	if err := bundle.Check(); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q: %w", bundle.Reference, err)
	}
	digest := bundle.Digest
	var manifest ocispec.Manifest
	if err := readLayoutDocument(bundle.Layout, digest, &manifest); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q manifest: %w", bundle.Reference, err)
	}
	var config ocispec.Image
	if err := readLayoutDocument(bundle.Layout, manifest.Config.Digest.String(), &config); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q config: %w", bundle.Reference, err)
	}
	read := bundleInspection{Digest: digest, Labels: config.Config.Labels}
	if err := admitBundle(read.Labels, admit); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q: %w", bundle.Reference, err)
	}
	return read, verifyBundle(bundle.Reference, digest, func() ([]v1.Layer, error) {
		return layoutLayers(bundle.Layout, manifest.Layers)
	}, verified)
}

// readLayoutDocument decodes the JSON blob digest names in layout, checked
// against that digest.
func readLayoutDocument(layout, digest string, into any) error {
	blob, err := devimage.OpenLayoutBlob(layout, digest)
	if err != nil {
		return err
	}
	defer blob.Close()
	data, err := io.ReadAll(io.LimitReader(blob, maxLayoutDocument+1))
	if err != nil {
		return err
	}
	if len(data) > maxLayoutDocument {
		return fmt.Errorf("%s is larger than %d bytes", digest, maxLayoutDocument)
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("%s holds bytes with digest %s", digest, got)
	}
	return json.Unmarshal(data, into)
}

// layoutLayers is a layout's layers, each read from the layout as it is
// opened. Their bytes are checked against their digests where they are served
// (resources/assets); here only what they hold is read.
func layoutLayers(layout string, descriptors []ocispec.Descriptor) ([]v1.Layer, error) {
	layers := make([]v1.Layer, 0, len(descriptors))
	for _, descriptor := range descriptors {
		digest := descriptor.Digest.String()
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return devimage.OpenLayoutBlob(layout, digest)
		})
		if err != nil {
			return nil, fmt.Errorf("read layer %s: %w", digest, err)
		}
		layers = append(layers, layer)
	}
	return layers, nil
}
