// Package assets serves bundles to pools by blob (ADR 26-10-16-423 §13): the
// manifests a pool's scope pins, and the blobs those manifests reference, from
// one content-addressed store the server fills from wherever a bundle is.
package assets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Options configures a Service.
type Options struct {
	// Dir is the blob store: an OCI layout holding blobs no entry names.
	Dir string
	// PlatformBundle is the platform bundle the release pins, which every pool
	// is answered.
	PlatformBundle string
	// ReleaseBundles are the bundle references the release names: the
	// platform's and the built-in harnesses'. Only these are pulled with the
	// server's registry credentials.
	ReleaseBundles []string
	// Development are the development bundles, read from their OCI image
	// layouts, which a development server's release references name.
	Development *devbundles.Set
}

// Service answers a pool's asset reads.
type Service struct {
	store    *store.Store
	blobs    *imagecache.Layout
	platform *platformPins
	release  map[string]bool
	// sources are where a blob the store lacks is looked for, in order;
	// resolvers, what a reference names now.
	sources   []source
	resolvers []source

	// fetches shares one fetch of a digest among every read wanting it.
	fetches singleflight.Group
	// manifests holds manifests already read, by digest: a digest names its
	// content, so an entry never goes stale.
	manifests sync.Map
	now       func() time.Time
}

// fetchTimeout bounds a fetch shared among reads, which runs past any one
// read's own cancellation so that the others are still answered.
const fetchTimeout = 10 * time.Minute

// New returns a Service storing blobs in opts.Dir, filled from the watcher's
// layouts and registries.
func New(store *store.Store, opts Options) *Service {
	layouts := layoutSource{bundles: opts.Development}
	registry := registrySource{}
	// The layouts first, as harness registration reads a bundle: what they
	// hold is what was built here.
	return newService(store, opts, []source{layouts, registry}, []source{layouts, registry})
}

func newService(store *store.Store, opts Options, sources, resolvers []source) *Service {
	release := map[string]bool{}
	for _, ref := range append([]string{opts.PlatformBundle}, opts.ReleaseBundles...) {
		if ref = strings.TrimSpace(ref); ref != "" {
			release[ref] = true
		}
	}
	s := &Service{
		store:     store,
		blobs:     imagecache.Open(opts.Dir),
		release:   release,
		sources:   sources,
		resolvers: resolvers,
		now:       time.Now,
	}
	s.platform = &platformPins{ref: strings.TrimSpace(opts.PlatformBundle), resolve: s.resolve, now: func() time.Time { return s.now() }}
	return s
}

// OnPlatformMoved has moved run, detached from any read, each time the
// release's platform reference resolves to a digest other than the one before,
// the first resolution included. Set it before the service is used.
func (s *Service) OnPlatformMoved(moved func(ctx context.Context)) {
	s.platform.moved = func(string) { moved(context.Background()) }
}

// ResolvePlatform resolves the release's platform reference now, so the
// sandboxes pinned to it follow a release the server starts on without
// waiting for a read to ask (ADR 26-10-16-423 §7). A reference pinned by
// digest never resolves, and is reported moved here instead.
func (s *Service) ResolvePlatform(ctx context.Context) {
	if s.platform.ref == "" {
		return
	}
	if s.platform.fixed() != "" {
		if s.platform.moved != nil {
			go s.platform.moved(s.platform.fixed())
		}
		return
	}
	if _, err := s.platform.pins(ctx, true); err != nil {
		slog.Warn("resolve the platform bundle", "reference", s.platform.ref, "error", err)
	}
}

// CurrentPlatform is the platform bundle the release's reference resolves
// to, asked again once the last answer is a minute old; zero when the server
// names none.
func (s *Service) CurrentPlatform(ctx context.Context) (services.BundlePin, error) {
	if s.platform.ref == "" {
		return services.BundlePin{}, nil
	}
	if _, err := s.platform.pins(ctx, false); err != nil {
		return services.BundlePin{}, err
	}
	return s.LatestPlatform(), nil
}

// LatestPlatform is what the release's platform reference last resolved to,
// without asking again; zero when it has not resolved.
func (s *Service) LatestPlatform() services.BundlePin {
	digest := s.platform.current()
	if digest == "" {
		return services.BundlePin{}
	}
	return services.BundlePin{Reference: s.platform.ref, Digest: digest}
}

// ResolveAssetName answers the manifest a bundle name in the pool's scope is
// pinned to now, for registration and re-pinning: the platform's, resolved
// afresh — a pool is answered whatever the release's reference names — or the
// digest a harness config of the pool's project records. A harness's tag that
// has moved since is not followed: its new digest is answered once it is
// re-pinned (ADR 0016), never before.
func (s *Service) ResolveAssetName(ctx context.Context, poolID, name string) (services.AssetContent, error) {
	projectID, err := s.authorize(ctx, poolID)
	if err != nil {
		return services.AssetContent{}, err
	}
	if s.platform.ref != "" && services.BundleName(s.platform.ref) == name {
		digest, err := s.platform.newest(ctx)
		if err != nil {
			return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, err.Error())
		}
		return s.openManifest(ctx, pin{ref: s.platform.ref, digest: digest, release: true}, digest)
	}
	pins, err := s.harnessPins(ctx, projectID)
	if err != nil {
		return services.AssetContent{}, err
	}
	var named []pin
	for _, p := range pins {
		if p.name() == name && !slices.ContainsFunc(named, func(other pin) bool { return other.digest == p.digest }) {
			named = append(named, p)
		}
	}
	switch len(named) {
	case 0:
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("no bundle named %q is pinned in this pool's scope", name))
	case 1:
		return s.openManifest(ctx, named[0], named[0].digest)
	}
	refs := make([]string, 0, len(named))
	for _, p := range named {
		refs = append(refs, p.ref+"@"+p.digest)
	}
	return services.AssetContent{}, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("bundle name %q names %d pins in this pool's scope: %s", name, len(named), strings.Join(refs, ", ")))
}

// AssetManifest answers a manifest by digest: a pin's own, or one its index lists,
// under the name of that pin.
func (s *Service) AssetManifest(ctx context.Context, poolID, name, digest string) (services.AssetContent, error) {
	if !imagecache.IsDigest(digest) {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("digest %q is not sha256:<64 hex>", digest))
	}
	pins, platformErr, err := s.scope(ctx, poolID)
	if err != nil {
		return services.AssetContent{}, err
	}
	for _, p := range pins {
		if p.name() != name {
			continue
		}
		if p.digest == digest {
			return s.openManifest(ctx, p, digest)
		}
		top, err := s.manifest(ctx, p, p.digest)
		if err != nil {
			slog.Warn("asset pin unreadable", "reference", p.ref, "digest", p.digest, "error", err)
			continue
		}
		for _, child := range top.Manifests {
			if child.Digest == digest {
				return s.openManifest(ctx, p, digest)
			}
		}
	}
	if platformErr != nil {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, platformErr.Error())
	}
	return services.AssetContent{}, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("manifest %s of %q is not pinned in this pool's scope", digest, name))
}

// AssetBlob answers a blob by digest when a manifest in the pool's scope
// references it.
func (s *Service) AssetBlob(ctx context.Context, poolID, digest string) (services.AssetContent, error) {
	if !imagecache.IsDigest(digest) {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("digest %q is not sha256:<64 hex>", digest))
	}
	pins, platformErr, err := s.scope(ctx, poolID)
	if err != nil {
		return services.AssetContent{}, err
	}
	for _, p := range pins {
		if s.references(ctx, p, digest) {
			if err := s.ensure(ctx, p, digest, false); err != nil {
				return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, fmt.Sprintf("fetch blob %s of %s: %s", digest, p.ref, err))
			}
			return s.open(digest, "")
		}
	}
	if platformErr != nil {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, platformErr.Error())
	}
	return services.AssetContent{}, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("blob %s is referenced by no manifest in this pool's scope", digest))
}

// references reports whether a manifest beneath p — its own, or under an
// index each manifest it lists — names digest as its config or a layer.
//
// What the store already holds is looked at first, and a listed manifest it
// lacks is fetched only when that did not find digest: under a development
// build's index the other platforms' manifests are nowhere, and asking every
// source for them on every read would cost each read a registry round trip.
// A listed manifest that cannot be read references nothing.
func (s *Service) references(ctx context.Context, p pin, digest string) bool {
	top, err := s.manifest(ctx, p, p.digest)
	if err != nil {
		slog.Warn("asset pin unreadable", "reference", p.ref, "digest", p.digest, "error", err)
		return false
	}
	if top.References(digest) {
		return true
	}
	var missing []string
	for _, child := range top.Manifests {
		if !s.has(child.Digest) {
			missing = append(missing, child.Digest)
			continue
		}
		if read, err := s.manifest(ctx, p, child.Digest); err == nil && read.References(digest) {
			return true
		}
	}
	for _, child := range missing {
		if read, err := s.manifest(ctx, p, child); err == nil && read.References(digest) {
			return true
		}
	}
	return false
}

// authorize holds a read to the pool its principal is, and returns that
// pool's project.
func (s *Service) authorize(ctx context.Context, poolID string) (string, error) {
	poolID = strings.TrimSpace(poolID)
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return "", apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to read this pool's assets")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return "", apperrors.NotFound(err, "pool not found")
	}
	return pool.ProjectID, nil
}

// openManifest makes a pin's manifest present and opens it, refusing content
// under that digest that is no manifest — an image ID a classic daemon gave,
// which is its config's digest.
func (s *Service) openManifest(ctx context.Context, p pin, digest string) (services.AssetContent, error) {
	read, err := s.manifest(ctx, p, digest)
	if err == nil {
		// Read before, it may have been pruned since.
		err = s.ensure(ctx, p, digest, true)
	}
	if err != nil {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, fmt.Sprintf("manifest %s of %s: %s", digest, p.ref, err))
	}
	return s.open(digest, read.MediaType)
}

func (s *Service) open(digest, mediaType string) (services.AssetContent, error) {
	body, size, err := s.blobs.OpenServedBlob(digest)
	if err != nil {
		return services.AssetContent{}, fmt.Errorf("open %s: %w", digest, err)
	}
	return services.AssetContent{MediaType: mediaType, Digest: digest, Size: size, Body: body}, nil
}

// ensure makes digest, a manifest or blob of p, present in the store, from
// the first source holding it. Reads wanting one digest share its fetch.
func (s *Service) ensure(ctx context.Context, p pin, digest string, isManifest bool) error {
	if s.has(digest) {
		return nil
	}
	_, err, _ := s.fetches.Do(digest, func() (any, error) {
		if s.has(digest) {
			return nil, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		var failures []error
		for _, src := range s.sources {
			err := src.fetch(fetchCtx, p, digest, isManifest, s.blobs)
			if err == nil && s.has(digest) {
				return nil, nil
			}
			if err != nil && !errors.Is(err, errNotHere) {
				failures = append(failures, err)
			}
		}
		if len(failures) == 0 {
			return nil, fmt.Errorf("no source holds %s", digest)
		}
		return nil, errors.Join(failures...)
	})
	return err
}

func (s *Service) has(digest string) bool { return s.blobs.HasBlob(digest) }

// resolve answers what ref names now, leaving its top-level manifest in the
// store.
func (s *Service) resolve(ctx context.Context, ref string) (string, error) {
	var failures []error
	for _, src := range s.resolvers {
		digest, err := src.resolve(ctx, ref, s.release[ref], s.blobs)
		if err == nil {
			return digest, nil
		}
		if !errors.Is(err, errNotHere) {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return "", fmt.Errorf("no source holds %s", ref)
	}
	return "", errors.Join(failures...)
}
