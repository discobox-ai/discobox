package imagecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// The asset stores — the server's, and each pool's pull-through cache of it
// (ADR 26-10-16-423 §13) — hold a bundle's manifests and blobs by digest and
// walk a pin's manifests to say what it reaches. What they share is here, so
// the two cannot read a manifest differently.

// IsDigest reports whether digest is one content is addressed by,
// sha256:<64 lowercase hex>.
func IsDigest(digest string) bool { return digestPattern.MatchString(digest) }

// Manifest is an index or an image manifest, as much of either as a pin's
// reach is walked by.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// ParseManifest reads an index or an image manifest, naming its media type
// when it states none, as an OCI one may not: an index by its manifests, an
// image manifest by its config. An image config is neither, although it has a
// config of its own: that one is no descriptor, and the schema version is
// absent.
func ParseManifest(data []byte) (Manifest, error) {
	var read Manifest
	if err := json.Unmarshal(data, &read); err != nil {
		return Manifest{}, fmt.Errorf("not a manifest: %w", err)
	}
	if read.SchemaVersion != 2 {
		return Manifest{}, fmt.Errorf("not a manifest: schema version %d", read.SchemaVersion)
	}
	hasConfig := read.Config != nil && IsDigest(read.Config.Digest)
	switch {
	case isIndex(read.MediaType):
		return read, nil
	case isManifest(read.MediaType):
		if !hasConfig {
			return Manifest{}, errors.New("an image manifest names no config")
		}
		return read, nil
	case read.MediaType == "":
		switch {
		case read.Manifests != nil:
			read.MediaType = MediaTypeOCIIndex
			return read, nil
		case hasConfig:
			read.MediaType = MediaTypeOCIManifest
			return read, nil
		}
	}
	return Manifest{}, fmt.Errorf("not a manifest: media type %q", read.MediaType)
}

// References reports whether an image manifest names digest as its config or
// one of its layers.
func (m Manifest) References(digest string) bool {
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

// Lists reports whether an index lists digest as one of its manifests.
func (m Manifest) Lists(digest string) bool {
	for _, child := range m.Manifests {
		if child.Digest == digest {
			return true
		}
	}
	return false
}

// ForPlatform is an index's image manifest for platform. Entries that are
// not images — the attestation manifests buildx publishes beside every
// platform, whose platform is unknown/unknown — never match.
func (m Manifest) ForPlatform(platform Platform) (Descriptor, bool) {
	for _, entry := range m.Manifests {
		if isManifest(entry.MediaType) && platform.matches(entry.Platform) {
			return entry, true
		}
	}
	return Descriptor{}, false
}

// ImageManifest resolves a pin to the image manifest it names for platform:
// the manifest at digest when it is one, or its index's entry for platform,
// each read with read. It returns that manifest and its digest. The pool's
// cache and a box's store resolve a platform bundle's pin this way, so the
// two cannot place and fetch different manifests.
func ImageManifest(digest string, platform Platform, read func(digest string) (Manifest, error)) (Manifest, string, error) {
	top, err := read(digest)
	if err != nil {
		return Manifest{}, "", err
	}
	if top.Config != nil {
		return top, digest, nil
	}
	child, ok := top.ForPlatform(platform)
	if !ok {
		return Manifest{}, "", fmt.Errorf("%s lists no %s image manifest", digest, platform)
	}
	read2, err := read(child.Digest)
	if err != nil {
		return Manifest{}, "", err
	}
	if read2.Config == nil {
		return Manifest{}, "", fmt.Errorf("%s's %s entry %s is not an image manifest", digest, platform, child.Digest)
	}
	return read2, child.Digest, nil
}

// ErrDamaged is a blob in a layout whose bytes are not its digest's.
var ErrDamaged = errors.New("damaged in the store")

// ReadManifestBlob reads the manifest blob digest whole. A blob whose bytes
// are not its digest's is an error wrapping ErrDamaged, for the caller to
// give up and fetch again.
func (l *Layout) ReadManifestBlob(digest string) ([]byte, error) {
	body, size, err := l.OpenBlob(digest)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	if size > MaxManifestBytes {
		return nil, fmt.Errorf("%s is %d bytes, more than a manifest is read to", digest, size)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", digest, ErrDamaged, err)
	}
	return data, nil
}

// HasBlob reports whether the layout holds the blob digest.
func (l *Layout) HasBlob(digest string) bool {
	body, _, err := l.OpenBlob(digest)
	if err != nil {
		return false
	}
	_ = body.Close()
	return true
}

// OpenServedBlob is OpenBlob for a blob about to be served: one whose bytes
// on disk turn out not to be its digest's is given up as the read that would
// end it fails, so the next read fetches it again rather than failing the
// same way. Its file is closed first: Windows does not remove a file that is
// open.
func (l *Layout) OpenServedBlob(digest string) (io.ReadCloser, int64, error) {
	body, size, err := l.OpenBlob(digest)
	if err != nil {
		return nil, 0, err
	}
	return &damagedBlob{ReadCloser: body, blobs: l, digest: digest}, size, nil
}

type damagedBlob struct {
	io.ReadCloser
	blobs  *Layout
	digest string
}

func (d *damagedBlob) Read(p []byte) (int, error) {
	n, err := d.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = d.Close()
		if removeErr := d.blobs.RemoveBlob(d.digest); removeErr != nil {
			slog.Warn("remove damaged blob", "digest", d.digest, "error", removeErr)
		}
	}
	return n, err
}

// Reach marks digest, and everything beneath it the layout holds, in keep:
// under an index each manifest it lists, under an image manifest its config
// and layers. Only what the layout holds is read, never fetched: a manifest it
// lacks has nothing beneath it here to keep.
func (l *Layout) Reach(digest string, keep map[string]bool) {
	if keep[digest] {
		return
	}
	keep[digest] = true
	data, err := l.ReadManifestBlob(digest)
	if err != nil {
		return
	}
	read, err := ParseManifest(data)
	if err != nil {
		return
	}
	for _, child := range read.Manifests {
		l.Reach(child.Digest, keep)
	}
	if read.Config != nil {
		keep[read.Config.Digest] = true
	}
	for _, layer := range read.Layers {
		keep[layer.Digest] = true
	}
}
