// Package assetcache is the pool's pull-through cache of bundles (ADR
// 26-10-16-423 §13): the blobs its sandboxes' pins reach, fetched from the
// server's asset API once and served to each sandbox at
// assets.discobox.internal, scoped to what that sandbox's pins name.
package assetcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// Fetch reads path, beneath the pool's asset prefix on the control plane
// (/{name}/manifests/{digest} or /blobs/{digest}), as this pool. The body is
// the caller's to close and to check against its digest.
type Fetch func(ctx context.Context, path string) (io.ReadCloser, error)

// Config configures a Cache.
type Config struct {
	// Dir is the blob store, in the pool's build area (ADR 0050): never bound
	// into a box.
	Dir string
	// Sandboxes is the parent of every sandbox's tree, where each one's
	// record (RecordFileName) is.
	Sandboxes string
	// Fetch is where a blob the cache lacks comes from: the control plane's
	// asset routes.
	Fetch Fetch
}

// Cache answers a sandbox's reads of the bundles it is pinned to.
type Cache struct {
	blobs     *imagecache.Layout
	sandboxes string
	fetch     Fetch

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

// New returns a Cache over cfg.Dir.
func New(cfg Config) *Cache {
	return &Cache{
		blobs:     imagecache.Open(cfg.Dir),
		sandboxes: cfg.Sandboxes,
		fetch:     cfg.Fetch,
		now:       time.Now,
	}
}

// errNotFound is a read nothing in the sandbox's scope answers. It is never
// asked of the server.
var errNotFound = errors.New("not pinned by this sandbox")

// pinnedManifest finds the sandbox's pin under which a manifest of name is in
// its scope: the pin's own digest, or one its index lists.
func (c *Cache) pinnedManifest(ctx context.Context, sandboxID, name, digest string) (Pin, error) {
	pins, err := sandboxPins(c.sandboxes, sandboxID)
	if err != nil {
		return Pin{}, err
	}
	for _, pin := range pins {
		if pin.Name != name {
			continue
		}
		if pin.Digest == digest {
			return pin, nil
		}
		top, err := c.manifest(ctx, pin.Name, pin.Digest)
		if err != nil {
			slog.Warn("asset pin unreadable", "name", pin.Name, "digest", pin.Digest, "error", err)
			continue
		}
		if top.Lists(digest) {
			return pin, nil
		}
	}
	return Pin{}, errNotFound
}

// pinnedBlob reports whether a manifest beneath one of the sandbox's pins —
// the pin's own, or under an index each manifest it lists — names digest as
// its config or a layer.
func (c *Cache) pinnedBlob(ctx context.Context, sandboxID, digest string) error {
	pins, err := sandboxPins(c.sandboxes, sandboxID)
	if err != nil {
		return err
	}
	for _, pin := range pins {
		top, err := c.manifest(ctx, pin.Name, pin.Digest)
		if err != nil {
			slog.Warn("asset pin unreadable", "name", pin.Name, "digest", pin.Digest, "error", err)
			continue
		}
		if top.References(digest) {
			return nil
		}
		for _, child := range top.Manifests {
			if read, err := c.manifest(ctx, pin.Name, child.Digest); err == nil && read.References(digest) {
				return nil
			}
		}
	}
	return errNotFound
}

// manifest reads digest, a manifest of the bundle name, fetching it into the
// store when it is not there. A manifest whose bytes on disk are not its
// digest's is given up and fetched again, once.
func (c *Cache) manifest(ctx context.Context, name, digest string) (imagecache.Manifest, error) {
	if cached, ok := c.manifests.Load(digest); ok {
		if read, ok := cached.(imagecache.Manifest); ok {
			return read, nil
		}
	}
	data, err := c.readManifest(ctx, name, digest)
	if errors.Is(err, imagecache.ErrDamaged) {
		if removeErr := c.blobs.RemoveBlob(digest); removeErr != nil {
			return imagecache.Manifest{}, errors.Join(err, removeErr)
		}
		data, err = c.readManifest(ctx, name, digest)
	}
	if err != nil {
		return imagecache.Manifest{}, err
	}
	read, err := imagecache.ParseManifest(data)
	if err != nil {
		return imagecache.Manifest{}, fmt.Errorf("%s: %w", digest, err)
	}
	c.manifests.Store(digest, read)
	return read, nil
}

func (c *Cache) readManifest(ctx context.Context, name, digest string) ([]byte, error) {
	if err := c.ensure(ctx, manifestPath(name, digest), digest); err != nil {
		return nil, err
	}
	return c.blobs.ReadManifestBlob(digest)
}

// manifestPath and blobPath are where the server answers a manifest and a
// blob, beneath the pool's asset prefix.
func manifestPath(name, digest string) string {
	return "/" + url.PathEscape(name) + "/manifests/" + digest
}

func blobPath(digest string) string { return "/blobs/" + digest }

// ensure makes digest present in the store, fetching it from the server's
// asset route at path. Reads wanting one digest share its fetch, which runs
// detached from the first reader's cancellation.
func (c *Cache) ensure(ctx context.Context, path, digest string) error {
	if c.has(digest) {
		return nil
	}
	_, err, _ := c.fetches.Do(digest, func() (any, error) {
		if c.has(digest) {
			return nil, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		return nil, c.fetchInto(fetchCtx, path, digest)
	})
	return err
}

// fetchInto puts the server's answer at path into the store under digest, which
// refuses bytes that are not that digest's.
func (c *Cache) fetchInto(ctx context.Context, path, digest string) error {
	body, err := c.fetch(ctx, path)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", digest, err)
	}
	defer body.Close()
	return c.blobs.PutBlob(digest, body)
}

func (c *Cache) has(digest string) bool { return c.blobs.HasBlob(digest) }
