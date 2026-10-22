package assets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/imagecache"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// pin is a bundle in a pool's scope: the reference it is named by, the
// manifest digest it is pinned to, and whether the release names it, which is
// what lets the server's own registry credentials pull it.
type pin struct {
	ref     string
	digest  string
	release bool
}

func (p pin) name() string { return services.BundleName(p.ref) }

// scope is every bundle a pool is answered: the platform bundle the release
// pins, each bundle a harness config of the pool's project is pinned to, and
// each a sandbox of it is (ADR 26-10-16-423 §§7, 13) — a sandbox keeps its
// pins after its config and the release have moved on.
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
	pins = append(pins, harnesses...)
	sandboxes, err := s.store.ListSandboxBundles(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("list sandbox bundles: %w", err)
	}
	for _, sb := range sandboxes {
		for _, p := range []pin{{ref: sb.PlatformBundle, digest: sb.PlatformBundleDigest}, {ref: sb.Bundle, digest: sb.BundleDigest}} {
			p.ref = strings.TrimSpace(p.ref)
			if p.ref == "" || !imagecache.IsDigest(p.digest) || slices.ContainsFunc(pins, func(other pin) bool { return other.ref == p.ref && other.digest == p.digest }) {
				continue
			}
			p.release = s.release[p.ref]
			pins = append(pins, p)
		}
	}
	return pins, platformErr, nil
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
		if ref == "" || !imagecache.IsDigest(config.BundleDigest) {
			continue
		}
		pins = append(pins, pin{ref: ref, digest: config.BundleDigest, release: s.release[ref]})
	}
	return pins, nil
}

// manifest reads digest, a manifest of p, fetching it into the store when it
// is not there. A manifest whose bytes on disk are not its digest's is given
// up and fetched again, once, as a damaged blob is (damagedBlob): it is what
// every read of the pin starts from, so one left in place would fail them all.
func (s *Service) manifest(ctx context.Context, p pin, digest string) (imagecache.Manifest, error) {
	if cached, ok := s.manifests.Load(digest); ok {
		if read, ok := cached.(imagecache.Manifest); ok {
			return read, nil
		}
	}
	data, err := s.readManifest(ctx, p, digest)
	if errors.Is(err, imagecache.ErrDamaged) {
		if removeErr := s.blobs.RemoveBlob(digest); removeErr != nil {
			return imagecache.Manifest{}, errors.Join(err, removeErr)
		}
		data, err = s.readManifest(ctx, p, digest)
	}
	if err != nil {
		return imagecache.Manifest{}, err
	}
	read, err := imagecache.ParseManifest(data)
	if err != nil {
		return imagecache.Manifest{}, fmt.Errorf("%s: %w", digest, err)
	}
	s.manifests.Store(digest, read)
	return read, nil
}

func (s *Service) readManifest(ctx context.Context, p pin, digest string) ([]byte, error) {
	if err := s.ensure(ctx, p, digest, true); err != nil {
		return nil, err
	}
	return s.blobs.ReadManifestBlob(digest)
}
