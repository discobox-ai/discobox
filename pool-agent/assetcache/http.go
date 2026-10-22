package assetcache

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/proxy"
)

// Handler serves assets.discobox.internal as the pool proxy forwards it:
// read-only, to the sandbox the proxy names in proxy.PoolClientHeader, and
// only what that sandbox's record pins. That header is its only word on who
// is asking, so it is served on a socket only the proxy reaches
// (layout.ProxyAssetsSocket), never a loopback port: a box can send its own
// header to one of those as ordinary egress (pool-agent/REVIEW.md, "A
// listener the proxy forwards to").
//
//	GET /{name}                       the sandbox's pin of name
//	GET /{name}/manifests/{digest}    a pinned manifest, or one a pinned index lists
//	GET /blobs/{digest}               a config or layer one of those references
//
// Anything else the sandbox asks for is a 404, and never asked of the server.
func (c *Cache) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "assets are read-only", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get(proxy.PoolClientHeader) == "" {
				http.Error(w, "the proxy names no sandbox", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		router.Method(method, "/blobs/{digest}", http.HandlerFunc(c.serveBlob))
		router.Method(method, "/{name}/manifests/{digest}", http.HandlerFunc(c.serveManifest))
		router.Method(method, "/{name}", http.HandlerFunc(c.serveName))
	}
	return router
}

func (c *Cache) serveName(w http.ResponseWriter, r *http.Request) {
	sandboxID, name := r.Header.Get(proxy.PoolClientHeader), chi.URLParam(r, "name")
	pins, err := sandboxPins(c.sandboxes, sandboxID)
	if err != nil {
		c.fail(w, err)
		return
	}
	for _, pin := range pins {
		if pin.Name == name {
			c.writeManifest(w, r, pin)
			return
		}
	}
	c.fail(w, errNotFound)
}

func (c *Cache) serveManifest(w http.ResponseWriter, r *http.Request) {
	sandboxID, name, digest := r.Header.Get(proxy.PoolClientHeader), chi.URLParam(r, "name"), chi.URLParam(r, "digest")
	if !imagecache.IsDigest(digest) {
		http.Error(w, fmt.Sprintf("digest %q is not sha256:<64 hex>", digest), http.StatusBadRequest)
		return
	}
	pin, err := c.pinnedManifest(r.Context(), sandboxID, name, digest)
	if err != nil {
		c.fail(w, err)
		return
	}
	c.writeManifest(w, r, Pin{Name: pin.Name, Digest: digest})
}

func (c *Cache) serveBlob(w http.ResponseWriter, r *http.Request) {
	sandboxID, digest := r.Header.Get(proxy.PoolClientHeader), chi.URLParam(r, "digest")
	if !imagecache.IsDigest(digest) {
		http.Error(w, fmt.Sprintf("digest %q is not sha256:<64 hex>", digest), http.StatusBadRequest)
		return
	}
	if err := c.pinnedBlob(r.Context(), sandboxID, digest); err != nil {
		c.fail(w, err)
		return
	}
	if err := c.ensure(r.Context(), blobPath(digest), digest); err != nil {
		c.fail(w, err)
		return
	}
	c.write(w, r, digest, "application/octet-stream")
}

// writeManifest answers a manifest in scope byte for byte, as its own media
// type.
func (c *Cache) writeManifest(w http.ResponseWriter, r *http.Request, pin Pin) {
	read, err := c.manifest(r.Context(), pin.Name, pin.Digest)
	if err == nil {
		// Read before, it may have been swept since.
		err = c.ensure(r.Context(), manifestPath(pin.Name, pin.Digest), pin.Digest)
	}
	if err != nil {
		c.fail(w, err)
		return
	}
	c.write(w, r, pin.Digest, read.MediaType)
}

// write streams a blob the store holds, checked against its digest as it goes.
// A blob that turns out damaged is removed and its response aborted, so the
// client sees one cut short of its Content-Length, never a complete one that is
// not the blob.
func (c *Cache) write(w http.ResponseWriter, r *http.Request, digest, mediaType string) {
	reader, size, err := c.blobs.OpenServedBlob(digest)
	if err != nil {
		c.fail(w, err)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, reader); err != nil {
		slog.Warn("serve asset blob", "digest", digest, "error", err)
		panic(http.ErrAbortHandler)
	}
}

func (c *Cache) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	slog.Warn("asset read failed", "error", err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}
