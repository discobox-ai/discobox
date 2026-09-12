// Package imagecachetest serves images the way a public registry does, for the
// tests of code that stages them into an image cache.
package imagecachetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	mediaTypeIndex    = "application/vnd.oci.image.index.v1+json"
	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
	mediaTypeLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Registry serves images the way ghcr.io does: every request is refused until
// it carries the anonymous bearer token its challenge points at, a tag resolves
// to a multi-platform index with an attestation manifest beside each platform,
// and blob fetches are counted so a test can tell what was downloaded.
type Registry struct {
	server *httptest.Server

	mu         sync.Mutex
	blobs      map[string][]byte
	mediaTypes map[string]string
	tags       map[string]string
	fetched    map[string]int
	corrupt    map[string]bool
	manifests  int
	// login, when set, is the username:password the token service demands.
	login string
}

// Image is what Publish put in the registry.
type Image struct {
	// Reference names the image by tag, at this registry.
	Reference string
	// Index is the digest of the index the tag resolves to: the image's
	// identity, which a staged copy has to keep.
	Index string
	// Configs and Layers are each platform's config and layer digests, keyed
	// by architecture.
	Configs map[string]string
	Layers  map[string][]string
}

// NewRegistry starts a registry that lives as long as the test.
func NewRegistry(t testing.TB) *Registry {
	t.Helper()
	return newRegistry(t, httptest.NewTLSServer)
}

// NewPlainRegistry starts a registry that speaks plain HTTP, as a development
// registry on this machine does.
func NewPlainRegistry(t testing.TB) *Registry {
	t.Helper()
	return newRegistry(t, httptest.NewServer)
}

func newRegistry(t testing.TB, start func(http.Handler) *httptest.Server) *Registry {
	t.Helper()
	r := &Registry{
		blobs:      map[string][]byte{},
		mediaTypes: map[string]string{},
		tags:       map[string]string{},
		fetched:    map[string]int{},
		corrupt:    map[string]bool{},
	}
	r.server = start(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

// Host is the registry's address, the first component of every reference
// Publish returns.
func (r *Registry) Host() string {
	return strings.TrimPrefix(strings.TrimPrefix(r.server.URL, "https://"), "http://")
}

// Client trusts the registry's certificate.
func (r *Registry) Client() *http.Client {
	return r.server.Client()
}

type descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int               `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
}

// encode is json.Marshal for the registry's own documents, which are built
// here from plain values and cannot fail to encode.
func encode(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func (r *Registry) add(data []byte, mediaType string) descriptor {
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[digest] = data
	r.mediaTypes[digest] = mediaType
	return descriptor{MediaType: mediaType, Digest: digest, Size: len(data)}
}

// Publish pushes repository:tag as an index over linux/amd64 and linux/arm64.
// Each platform has its own config, the shared layer every image published
// with the same bytes has, and a layer of its own.
func (r *Registry) Publish(repository, tag string, shared []byte) Image {
	image := Image{
		Reference: r.Host() + "/" + repository + ":" + tag,
		Configs:   map[string]string{},
		Layers:    map[string][]string{},
	}
	sharedLayer := r.add(shared, mediaTypeLayer)
	var entries []descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		config := r.add(fmt.Appendf(nil, `{"os":"linux","architecture":%q,"rootfs":{"type":"layers"},"image":%q}`, arch, repository), mediaTypeConfig)
		own := r.add(bytes.Repeat([]byte(repository+arch), 1000), mediaTypeLayer)
		entry := r.add(encode(manifest{
			SchemaVersion: 2, MediaType: mediaTypeManifest,
			Config: config, Layers: []descriptor{sharedLayer, own},
		}), mediaTypeManifest)
		entry.Platform = map[string]string{"os": "linux", "architecture": arch}
		entries = append(entries, entry)
		image.Configs[arch] = config.Digest
		image.Layers[arch] = []string{sharedLayer.Digest, own.Digest}
		// buildx publishes a provenance manifest beside every platform, under a
		// platform that matches nothing.
		entry = r.add(encode(manifest{
			SchemaVersion: 2, MediaType: mediaTypeManifest,
			Config: r.add([]byte(`{"attestation":"`+arch+`"}`), mediaTypeConfig), Layers: []descriptor{},
		}), mediaTypeManifest)
		entry.Platform = map[string]string{"os": "unknown", "architecture": "unknown"}
		entries = append(entries, entry)
	}
	top := r.add(encode(index{SchemaVersion: 2, MediaType: mediaTypeIndex, Manifests: entries}), mediaTypeIndex)
	r.mu.Lock()
	r.tags[repository+":"+tag] = top.Digest
	r.mu.Unlock()
	image.Index = top.Digest
	return image
}

// RequireLogin makes the token service refuse anyone who does not log in as
// username with password, as a private repository's does.
func (r *Registry) RequireLogin(username, password string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.login = username + ":" + password
}

// ManifestFetches is how many manifests the registry has served, which is how
// a test tells a staging that asked the registry from one that did not.
func (r *Registry) ManifestFetches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manifests
}

// Corrupt makes the registry serve different bytes under digest.
func (r *Registry) Corrupt(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.corrupt[digest] = true
}

// Fetched is how many times the blob named digest was downloaded.
func (r *Registry) Fetched(digest string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fetched[digest]
}

// Fetches is how many blob downloads the registry has served in all.
func (r *Registry) Fetches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, n := range r.fetched {
		total += n
	}
	return total
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		if !strings.HasPrefix(req.URL.Query().Get("scope"), "repository:") {
			http.Error(w, "no scope", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		login := r.login
		r.mu.Unlock()
		if username, password, ok := req.BasicAuth(); login != "" && (!ok || username+":"+password != login) {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"anonymous"}`))
		return
	}
	if req.Header.Get("Authorization") != "Bearer anonymous" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:x:pull"`, r.server.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	r.mu.Lock()
	defer r.mu.Unlock()
	if repository, reference, ok := strings.Cut(path, "/manifests/"); ok {
		digest := reference
		if !strings.HasPrefix(reference, "sha256:") {
			digest = r.tags[repository+":"+reference]
		}
		data, ok := r.blobs[digest]
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.manifests++
		w.Header().Set("Content-Type", r.mediaTypes[digest])
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = w.Write(data)
		return
	}
	if _, digest, ok := strings.Cut(path, "/blobs/"); ok {
		data, ok := r.blobs[digest]
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.fetched[digest]++
		if r.corrupt[digest] {
			data = bytes.ToUpper(data)
		}
		_, _ = w.Write(data)
		return
	}
	http.NotFound(w, req)
}
