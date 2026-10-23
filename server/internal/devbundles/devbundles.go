// Package devbundles is a development server's bundles (ADR 26-10-16-423
// §13): the ones the image watcher exported as OCI image layouts, and, in
// build-mode, the ones it only describes, which a pool's daemon builds and the
// server saves as layouts of its own. The asset store serves both, and harness
// seeding reads their labels, from here.
package devbundles

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// Set is the development bundles, by reference. A nil Set holds none, which
// is what a server with no development manifest has.
type Set struct {
	// dir is where a build-mode bundle's layout is saved, named by its
	// reference, which is content-addressed over the build's inputs: a layout
	// saved there by an earlier run is still that bundle's.
	dir string

	mu      sync.Mutex
	bundles map[string]devimage.Bundle
	onBuilt []func()
}

// New is the set of bundles, saving build-mode ones under dir. A build-mode
// bundle whose layout an earlier run saved is a layout already; a layout
// saved for a bundle the set no longer names — a build an edit has since
// superseded — is removed, so dir holds only what is served.
func New(bundles []devimage.Bundle, dir string) *Set {
	if len(bundles) == 0 {
		return nil
	}
	s := &Set{dir: dir, bundles: make(map[string]devimage.Bundle, len(bundles))}
	defer s.prune()
	for _, bundle := range bundles {
		if bundle.Build != nil {
			if saved, err := s.saved(bundle.Reference); err == nil {
				bundle = saved
			} else if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("read a saved development bundle; it is built again", "bundle", bundle.Reference, "error", err)
			}
		}
		s.bundles[bundle.Reference] = bundle
	}
	return s
}

// Layout is ref's bundle when it has a layout to be read from: exported by
// the watcher, or saved once a pool built it.
func (s *Set) Layout(ref string) (devimage.Bundle, bool) {
	bundle, ok := s.get(ref)
	return bundle, ok && bundle.Build == nil
}

// Unbuilt is ref's bundle while it is only a build description, which no
// pool has built yet.
func (s *Set) Unbuilt(ref string) (devimage.Bundle, bool) {
	bundle, ok := s.get(ref)
	return bundle, ok && bundle.Build != nil
}

// Builds is every bundle still to be built by a pool's daemon.
func (s *Set) Builds() []devimage.Bundle {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []devimage.Bundle
	for _, bundle := range s.bundles {
		if bundle.Build != nil {
			out = append(out, bundle)
		}
	}
	return out
}

// OnBuilt has built run, detached from whatever saved it, each time a bundle
// is saved. Set it before the set is used.
func (s *Set) OnBuilt(built func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onBuilt = append(s.onBuilt, built)
}

// Save records the OCI image layout tar read from layout as ref's bundle,
// which a pool's daemon built from its description. A layout a daemon saves
// may list its image through a nested index, as the containerd image store's
// does; it is saved listing the one image manifest that index holds, which is
// what the bundle is pinned by. A bundle another pool's daemon saved first,
// both having built it at once, is saved already, and this is no error.
func (s *Set) Save(ref string, layout io.Reader) error {
	if _, ok := s.Unbuilt(ref); !ok {
		if bundle, built := s.get(ref); built && bundle.Build == nil && bundle.Layout != "" && filepath.Dir(bundle.Layout) == s.dir {
			return nil
		}
		return fmt.Errorf("%s is no development bundle a pool builds", ref)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create development bundle directory: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".bundle-*.tar")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := writeFlattened(tmp, layout); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save development bundle %s: %w", ref, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Under the lock from here: two saves of one bundle that both passed the
	// check above race to this point, and the first to arrive is the one
	// kept, so the bundle is pinned by one digest and seeded once.
	s.mu.Lock()
	defer s.mu.Unlock()
	if bundle := s.bundles[ref]; bundle.Build == nil {
		return nil
	}
	if err := os.Rename(tmpPath, s.path(ref)); err != nil {
		return err
	}
	saved, err := s.saved(ref)
	if err != nil {
		return fmt.Errorf("save development bundle %s: %w", ref, err)
	}
	s.bundles[ref] = saved
	built := append([]func(){}, s.onBuilt...)
	slog.Info("saved a development bundle a pool built", "bundle", ref, "digest", saved.Digest)
	for _, fn := range built {
		go fn()
	}
	return nil
}

func (s *Set) get(ref string) (devimage.Bundle, bool) {
	if s == nil {
		return devimage.Bundle{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bundle, ok := s.bundles[ref]
	return bundle, ok
}

// prune removes from dir every layout no bundle of the set names, and any
// save cut short.
func (s *Set) prune() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for ref, bundle := range s.bundles {
		if bundle.Build != nil || filepath.Dir(bundle.Layout) == s.dir {
			keep[filepath.Base(s.path(ref))] = true
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || keep[entry.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
			slog.Warn("remove a superseded development bundle", "file", entry.Name(), "error", err)
		}
	}
}

// path is where ref's saved layout is: named by a digest of the reference,
// which a tag's characters cannot all be in a file name.
func (s *Set) path(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".tar")
}

// saved is ref's bundle read from its saved layout.
func (s *Set) saved(ref string) (devimage.Bundle, error) {
	path := s.path(ref)
	if _, err := os.Stat(path); err != nil {
		return devimage.Bundle{}, err
	}
	digest, err := devimage.LayoutDigest(path)
	if err != nil {
		return devimage.Bundle{}, err
	}
	return devimage.Bundle{Reference: ref, Digest: digest, Layout: path}, nil
}

// maxDocument bounds an index read whole while a layout is flattened.
const maxDocument = 4 << 20

// descriptor is the part of an OCI descriptor flattening reads.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

// writeFlattened copies the layout tar in to out with an index.json listing
// the one image manifest it holds: the one its index lists, or the one image
// of a nested index it lists, attestation manifests aside.
func writeFlattened(out io.Writer, in io.Reader) error {
	entries := tar.NewReader(in)
	written := tar.NewWriter(out)
	indexes := map[string][]byte{}
	var top []byte
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		if name == "index.json" {
			if top, err = io.ReadAll(io.LimitReader(entries, maxDocument)); err != nil {
				return err
			}
			continue
		}
		var body io.Reader = entries
		if header.Typeflag == tar.TypeReg && header.Size <= maxDocument {
			data, err := io.ReadAll(entries)
			if err != nil {
				return err
			}
			if hexDigest, ok := cutBlob(name); ok {
				indexes["sha256:"+hexDigest] = data
			}
			body = bytes.NewReader(data)
		}
		if err := written.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.Copy(written, body); err != nil {
			return err
		}
	}
	if top == nil {
		return errors.New("the layout has no index.json")
	}
	manifest, err := imageManifest(top, indexes)
	if err != nil {
		return err
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     imagecache.MediaTypeOCIIndex,
		"manifests":     []descriptor{{MediaType: manifest.MediaType, Digest: manifest.Digest, Size: manifest.Size}},
	})
	if err != nil {
		return err
	}
	if err := written.WriteHeader(&tar.Header{Name: "index.json", Mode: 0o644, Size: int64(len(index)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := written.Write(index); err != nil {
		return err
	}
	return written.Close()
}

// imageManifest is the one image manifest the index document lists, through
// at most one nested index whose blob is in blobs.
func imageManifest(document []byte, blobs map[string][]byte) (descriptor, error) {
	for depth := 0; depth < 2; depth++ {
		var index struct {
			Manifests []descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(document, &index); err != nil {
			return descriptor{}, fmt.Errorf("decode the layout's index: %w", err)
		}
		var images, nested []descriptor
		for _, entry := range index.Manifests {
			switch entry.MediaType {
			case imagecache.MediaTypeOCIManifest, imagecache.MediaTypeDockerManifest:
				// An attestation manifest names no platform of its own.
				if entry.Platform == nil || entry.Platform.OS != "unknown" {
					images = append(images, entry)
				}
			case imagecache.MediaTypeOCIIndex, imagecache.MediaTypeDockerList:
				nested = append(nested, entry)
			}
		}
		switch {
		case len(images) == 1 && len(nested) == 0:
			return images[0], nil
		case len(images) == 0 && len(nested) == 1:
			next, ok := blobs[nested[0].Digest]
			if !ok {
				return descriptor{}, fmt.Errorf("the layout lists the index %s and holds no blob of it", nested[0].Digest)
			}
			document = next
		default:
			return descriptor{}, fmt.Errorf("the layout lists %d images and %d indexes, not one bundle's", len(images), len(nested))
		}
	}
	return descriptor{}, errors.New("the layout nests its image deeper than one index")
}

func cutBlob(name string) (string, bool) {
	const prefix = "blobs/sha256/"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return "", false
	}
	return name[len(prefix):], true
}
