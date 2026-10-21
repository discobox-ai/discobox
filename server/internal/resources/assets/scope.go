package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// pin is a bundle in a pool's scope: the reference it is named by, the
// manifest digest it is pinned to, and whether the release names it, which is
// what lets the server's own registry credentials pull it.
type pin struct {
	ref     string
	digest  string
	release bool
}

func (p pin) name() string { return bundleName(p.ref) }

// bundleName is the name a bundle is addressed by: the last component of its
// reference's repository, so ghcr.io/discobox-ai/discobox-platform:v1 and a
// development build's discobox-platform:local are both discobox-platform.
// Empty for a reference that does not parse.
func bundleName(ref string) string {
	parsed, err := name.ParseReference(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	return path.Base(parsed.Context().RepositoryStr())
}

// scope is every bundle a pool is answered: the platform bundle the release
// pins, and each bundle a harness config of the pool's project is pinned to
// (ADR 26-10-16-423 §13). A sandbox pins no bundle of its own: it runs its
// harness's runnable image.
//
// platformErr says why the platform is missing from it, when it is, so that a
// read nothing else in scope answers fails as the server's fault rather than
// as one out of scope.
func (s *Service) scope(ctx context.Context, poolID string) (pins []pin, platformErr error, err error) {
	projectID, err := s.authorize(ctx, poolID)
	if err != nil {
		return nil, nil, err
	}
	digests, platformErr := s.platform.pins(ctx, false)
	for _, digest := range digests {
		pins = append(pins, pin{ref: s.platform.ref, digest: digest, release: true})
	}
	harnesses, err := s.harnessPins(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	return append(pins, harnesses...), platformErr, nil
}

// harnessPins is the bundle each harness config of a project is pinned to. A
// pin whose digest is no content digest — a build-mode development image,
// which is its own reference until a pool's daemon builds it — has no content
// here, and is left out.
func (s *Service) harnessPins(ctx context.Context, projectID string) ([]pin, error) {
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list harness configs: %w", err)
	}
	var pins []pin
	for _, config := range configs {
		ref := strings.TrimSpace(config.Bundle)
		if ref == "" || !digestPattern.MatchString(config.BundleDigest) {
			continue
		}
		pins = append(pins, pin{ref: ref, digest: config.BundleDigest, release: s.release[ref]})
	}
	return pins, nil
}

// Media types a manifest is served as.
const (
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// maxManifestBytes bounds a manifest read. The largest real indexes are tens
// of kilobytes; anything near this is not one.
const maxManifestBytes = 4 << 20

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// manifest is an index or an image manifest, as much of either as a scope is
// walked by.
type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// references reports whether an image manifest names digest as its config or
// one of its layers.
func (m manifest) references(digest string) bool {
	if m.Config != nil && m.Config.Digest == digest {
		return true
	}
	for _, layer := range m.Layers {
		if layer.Digest == digest {
			return true
		}
	}
	return false
}

// manifest reads digest, a manifest of p, fetching it into the store when it
// is not there. A manifest whose bytes on disk are not its digest's is given
// up and fetched again, once, as a damaged blob is (damagedBlob): it is what
// every read of the pin starts from, so one left in place would fail them all.
func (s *Service) manifest(ctx context.Context, p pin, digest string) (manifest, error) {
	if cached, ok := s.manifests.Load(digest); ok {
		if read, ok := cached.(manifest); ok {
			return read, nil
		}
	}
	data, err := s.readManifest(ctx, p, digest)
	if errors.Is(err, errDamaged) {
		if removeErr := s.blobs.RemoveBlob(digest); removeErr != nil {
			return manifest{}, errors.Join(err, removeErr)
		}
		data, err = s.readManifest(ctx, p, digest)
	}
	if err != nil {
		return manifest{}, err
	}
	read, err := parseManifest(data)
	if err != nil {
		return manifest{}, fmt.Errorf("%s: %w", digest, err)
	}
	s.manifests.Store(digest, read)
	return read, nil
}

// errDamaged is a blob in the store whose bytes are not its digest's.
var errDamaged = errors.New("damaged in the store")

func (s *Service) readManifest(ctx context.Context, p pin, digest string) ([]byte, error) {
	if err := s.ensure(ctx, p, digest, true); err != nil {
		return nil, err
	}
	body, size, err := s.blobs.OpenBlob(digest)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	if size > maxManifestBytes {
		return nil, fmt.Errorf("%s is %d bytes, more than a manifest is read to", digest, size)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", digest, errDamaged, err)
	}
	return data, nil
}

// parseManifest reads an index or an image manifest, naming its media type
// when it states none, as an OCI one may not: an index by its manifests, an
// image manifest by its config. An image config is neither, although it has a
// config of its own: that one is no descriptor, and the schema version is
// absent.
func parseManifest(data []byte) (manifest, error) {
	var read manifest
	if err := json.Unmarshal(data, &read); err != nil {
		return manifest{}, fmt.Errorf("not a manifest: %w", err)
	}
	if read.SchemaVersion != 2 {
		return manifest{}, fmt.Errorf("not a manifest: schema version %d", read.SchemaVersion)
	}
	hasConfig := read.Config != nil && digestPattern.MatchString(read.Config.Digest)
	switch read.MediaType {
	case mediaTypeOCIIndex, mediaTypeDockerList:
		return read, nil
	case mediaTypeOCIManifest, mediaTypeDockerManifest:
		if !hasConfig {
			return manifest{}, fmt.Errorf("an image manifest names no config")
		}
		return read, nil
	case "":
		switch {
		case read.Manifests != nil:
			read.MediaType = mediaTypeOCIIndex
			return read, nil
		case hasConfig:
			read.MediaType = mediaTypeOCIManifest
			return read, nil
		}
	}
	return manifest{}, fmt.Errorf("not a manifest: media type %q", read.MediaType)
}
