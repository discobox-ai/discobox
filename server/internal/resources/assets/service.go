// Package assets serves bundles to pools by blob (ADR 26-10-16-423 §13): the
// manifests a pool's scope pins, and the blobs those manifests reference, from
// one content-addressed store the server fills from wherever a bundle is.
package assets

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Options configures a Service.
type Options struct {
	// Dir is the blob store: an OCI layout holding blobs no entry names.
	Dir string
	// PlatformBundle, AgentBundle and PoolBundle are the platform, agent and
	// pool bundles the release pins, which every pool is answered: what a
	// sandbox applies, what every box is placed with, and what a pool's box
	// applies (ADR 26-10-24-953 §§1–2).
	PlatformBundle string
	AgentBundle    string
	PoolBundle     string
	// ReleaseBundles are the other bundle references the release names: the
	// built-in harnesses' and the pool's. Only these, the platform and the
	// agent are pulled with the server's registry credentials.
	ReleaseBundles []string
	// Development are the development bundles, read from their OCI image
	// layouts, which a development server's release references name.
	Development *devbundles.Set
}

// Service answers a pool's asset reads.
type Service struct {
	store    *store.Store
	blobs    *imagecache.Layout
	platform *releasePins
	agent    *releasePins
	pool     *releasePins
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
	for _, ref := range append([]string{opts.PlatformBundle, opts.AgentBundle, opts.PoolBundle}, opts.ReleaseBundles...) {
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
	s.platform = &releasePins{what: "platform bundle", ref: strings.TrimSpace(opts.PlatformBundle), resolve: s.resolve, now: func() time.Time { return s.now() }}
	s.agent = &releasePins{what: "agent bundle", ref: strings.TrimSpace(opts.AgentBundle), resolve: s.resolve, now: func() time.Time { return s.now() }}
	s.pool = &releasePins{what: "pool bundle", ref: strings.TrimSpace(opts.PoolBundle), resolve: s.resolve, now: func() time.Time { return s.now() }}
	return s
}

// releaseBundles is each bundle the release pins every pool is answered.
func (s *Service) releaseBundles() []*releasePins {
	return []*releasePins{s.platform, s.agent, s.pool}
}

// OnReleaseMoved has moved run, detached from any read, each time one of
// the release's bundle references — the platform's, the agent's, the pool's —
// resolves to a digest other than the one before, the first resolution
// included. Set it before the service is used.
func (s *Service) OnReleaseMoved(moved func(ctx context.Context)) {
	for _, bundle := range s.releaseBundles() {
		bundle.moved = func(string) { moved(context.Background()) }
	}
}

// ResolveRelease resolves the release's bundle references now, so the
// sandboxes pinned to them follow a release the server starts on without
// waiting for a read to ask (ADR 26-10-16-423 §7). A reference pinned by
// digest never resolves, and is reported moved here instead.
func (s *Service) ResolveRelease(ctx context.Context) {
	for _, bundle := range s.releaseBundles() {
		if bundle.ref == "" {
			continue
		}
		if bundle.fixed() != "" {
			if bundle.moved != nil {
				go bundle.moved(bundle.fixed())
			}
			continue
		}
		if _, err := bundle.pins(ctx, true); err != nil {
			slog.Warn("resolve a release bundle", "bundle", bundle.what, "reference", bundle.ref, "error", err)
		}
	}
}

// CurrentRelease is the bundles the release's references resolve to that a
// sandbox is pinned to, each asked again once its last answer is a minute
// old; each zero when the server names none.
func (s *Service) CurrentRelease(ctx context.Context) (services.ReleasePins, error) {
	for _, bundle := range []*releasePins{s.platform, s.agent} {
		if bundle.ref == "" {
			continue
		}
		if _, err := bundle.pins(ctx, false); err != nil {
			return services.ReleasePins{}, err
		}
	}
	return s.LatestRelease(), nil
}

// LatestRelease is what the release's references last resolved to, without
// asking again; each zero when it has not resolved.
func (s *Service) LatestRelease() services.ReleasePins {
	return services.ReleasePins{Platform: latest(s.platform), Agent: latest(s.agent)}
}

func latest(bundle *releasePins) model.BundlePin {
	digest := bundle.current()
	if digest == "" {
		return model.BundlePin{}
	}
	return model.BundlePin{Reference: bundle.ref, Digest: digest}
}

// PoolBox is what a pool's box is placed with (ADR 26-10-24-953 §2, ADR
// 26-10-30-875 §5): the release's agent bundle and its one layer for
// platform, the release's pool bundle as the pool's one pin, and every
// manifest and layer of it the box's content store reads for platform. It
// fetches what the store lacks of both bundles' manifests, and reads a layer
// only when OpenBlob is called.
func (s *Service) PoolBox(ctx context.Context, platform imagecache.Platform) (sandbox.PoolBox, error) {
	var pins []pin
	for _, bundle := range []*releasePins{s.agent, s.pool} {
		if bundle.ref == "" {
			return sandbox.PoolBox{}, fmt.Errorf("this server names no %s, which every pool's box is placed with", bundle.what)
		}
		if _, err := bundle.pins(ctx, false); err != nil {
			return sandbox.PoolBox{}, err
		}
		resolved := latest(bundle)
		if resolved.Digest == "" {
			return sandbox.PoolBox{}, fmt.Errorf("the %s %s has not resolved", bundle.what, bundle.ref)
		}
		pins = append(pins, pin{ref: resolved.Reference, digest: resolved.Digest, release: true})
	}
	agent, pool := pins[0], pins[1]
	// owners is what pin each blob the box is placed with is fetched as, and
	// whether it is a manifest.
	type owner struct {
		pin      pin
		manifest bool
	}
	owners := map[string]owner{}
	read, _, err := imagecache.ImageManifest(agent.digest, platform, func(digest string) (imagecache.Manifest, error) {
		return s.manifest(ctx, agent, digest)
	})
	if err != nil {
		return sandbox.PoolBox{}, fmt.Errorf("the agent bundle %s for %s: %w", agent.ref, platform, err)
	}
	if len(read.Layers) != 1 {
		return sandbox.PoolBox{}, fmt.Errorf("the agent bundle %s for %s is %d layers, not the agent's one", agent.ref, platform, len(read.Layers))
	}
	agentLayer := read.Layers[0].Digest
	owners[agentLayer] = owner{pin: agent}
	var blobs []string
	read, _, err = imagecache.ImageManifest(pool.digest, platform, func(digest string) (imagecache.Manifest, error) {
		if _, ok := owners[digest]; !ok {
			owners[digest] = owner{pin: pool, manifest: true}
			blobs = append(blobs, digest)
		}
		return s.manifest(ctx, pool, digest)
	})
	if err != nil {
		return sandbox.PoolBox{}, fmt.Errorf("the pool bundle %s for %s: %w", pool.ref, platform, err)
	}
	for _, layer := range read.Layers {
		if !imagecache.IsDigest(layer.Digest) {
			return sandbox.PoolBox{}, fmt.Errorf("the pool bundle %s for %s names a layer %q that is not a sha256 digest", pool.ref, platform, layer.Digest)
		}
		if _, ok := owners[layer.Digest]; !ok {
			owners[layer.Digest] = owner{pin: pool}
			blobs = append(blobs, layer.Digest)
		}
	}
	return sandbox.PoolBox{
		Agent:      sandbox.BundlePin{Name: agent.name(), Digest: agent.digest},
		AgentLayer: agentLayer,
		Bundles:    []sandbox.BundlePin{{Name: pool.name(), Digest: pool.digest}},
		Blobs:      blobs,
		OpenBlob: func(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
			owner, ok := owners[digest]
			if !ok {
				return nil, 0, fmt.Errorf("%s is no blob of a pool's box", digest)
			}
			if err := s.ensure(ctx, owner.pin, digest, owner.manifest); err != nil {
				return nil, 0, fmt.Errorf("fetch %s of %s: %w", digest, owner.pin.ref, err)
			}
			content, err := s.open(digest, "")
			if err != nil {
				return nil, 0, err
			}
			return content.Body, content.Size, nil
		},
	}, nil
}

// ResolveAssetName answers the manifest a bundle name in the pool's scope is
// pinned to now, for registration and re-pinning: a release bundle's,
// resolved afresh — a pool is answered whatever the release's reference
// names — or the
// digest a harness config of the pool's project records. A harness's tag that
// has moved since is not followed: its new digest is answered once it is
// re-pinned (ADR 0016), never before.
func (s *Service) ResolveAssetName(ctx context.Context, poolID, name string) (services.AssetContent, error) {
	projectID, err := s.authorize(ctx, poolID)
	if err != nil {
		return services.AssetContent{}, err
	}
	for _, bundle := range s.releaseBundles() {
		if bundle.ref == "" || model.BundleName(bundle.ref) != name {
			continue
		}
		digest, err := bundle.newest(ctx)
		if err != nil {
			return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, err.Error())
		}
		return s.openManifest(ctx, pin{ref: bundle.ref, digest: digest, release: true}, digest)
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
	pins, releaseErr, err := s.scope(ctx, poolID)
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
	if releaseErr != nil {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, releaseErr.Error())
	}
	return services.AssetContent{}, apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("manifest %s of %q is not pinned in this pool's scope", digest, name))
}

// AssetBlob answers a blob by digest when a manifest in the pool's scope
// references it.
func (s *Service) AssetBlob(ctx context.Context, poolID, digest string) (services.AssetContent, error) {
	if !imagecache.IsDigest(digest) {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("digest %q is not sha256:<64 hex>", digest))
	}
	pins, releaseErr, err := s.scope(ctx, poolID)
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
	if releaseErr != nil {
		return services.AssetContent{}, apperrors.NewStatusError(http.StatusBadGateway, releaseErr.Error())
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
