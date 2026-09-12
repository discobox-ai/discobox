// Package imagecache keeps container images on this machine as an OCI image
// layout, so a pool can load them from disk instead of pulling them from their
// registry (ADR 0113).
//
// Two processes share one layout and meet only in its files. The CLI stages
// into it: each reference is resolved against its registry once, for the one
// platform a pool on this machine runs, and every blob the layout does not
// already hold is downloaded and checked against its digest before it is
// renamed into place. The server reads from it: Lookup finds a reference that
// is completely staged for a daemon's platform, and WriteArchive streams it in
// the form `docker load` takes.
//
// The registry's own top-level manifest is kept byte for byte — for a
// multi-platform image, its index — although only one platform's content is
// staged beneath it. Its digest is what a harness config pins a sandbox to (ADR
// 0016 §6), and an image that loads under any other identity is one the pool
// agent refuses to run.
package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
)

const (
	layoutFileName = "oci-layout"
	indexFileName  = "index.json"
	layoutVersion  = `{"imageLayoutVersion":"1.0.0"}`
)

const (
	// AnnotationImageName is the reference an entry was staged for, fully
	// qualified. It is containerd's own key, which is what makes an archive
	// written from the entry load under this name.
	AnnotationImageName = "io.containerd.image.name"
	// AnnotationRefName is the tag: the OCI layout's own way of naming an
	// entry, for tools that read the layout without knowing about this package.
	AnnotationRefName = "org.opencontainers.image.ref.name"
	// annotationStaged is when a staging last asked for an entry, which is the
	// age pruning measures.
	annotationStaged = "io.discobox.image-cache.staged"
)

const (
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// maxManifestBytes bounds a manifest read, from a registry or from disk. The
// largest real indexes are tens of kilobytes; anything near this is not one.
const maxManifestBytes = 4 << 20

// maxConfigBytes bounds reading an image config to learn its platform.
const maxConfigBytes = 8 << 20

// ErrNotStaged reports a reference the layout does not hold all of.
var ErrNotStaged = errors.New("not in the image cache")

// Platform is an OCI platform.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// PoolPlatform is the platform a pool on this machine runs: Linux, whatever
// this machine runs, on this machine's architecture. Every provider that runs a
// pool here — docker, libkrun, vz, wslc — puts it on a Linux kernel of the
// host's architecture, which is the rule harness image inspection already
// follows.
func PoolPlatform() Platform {
	return Platform{OS: "linux", Architecture: runtime.GOARCH}
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// matches reports whether an index entry's platform is p. A variant is only
// compared when both sides state one: arm64 is published as v8 or with no
// variant at all, and a pool asking for arm64 wants either.
func (p Platform) matches(other *Platform) bool {
	if other == nil || other.OS != p.OS || other.Architecture != p.Architecture {
		return false
	}
	return p.Variant == "" || other.Variant == "" || p.Variant == other.Variant
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type imageIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Manifests     []Descriptor `json:"manifests"`
}

type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

func isIndex(mediaType string) bool {
	return mediaType == mediaTypeOCIIndex || mediaType == mediaTypeDockerList
}

func isManifest(mediaType string) bool {
	return mediaType == mediaTypeOCIManifest || mediaType == mediaTypeDockerManifest
}

// Layout is an OCI image layout on disk.
type Layout struct {
	dir string
}

// Open names a layout. Nothing is read or created until it is used, so a layout
// that does not exist yet is simply one that holds nothing.
func Open(dir string) *Layout {
	return &Layout{dir: filepath.Clean(dir)}
}

// Dir is the layout's directory.
func (l *Layout) Dir() string {
	return l.dir
}

func (l *Layout) blobDir() string {
	return filepath.Join(l.dir, "blobs", "sha256")
}

// blobPath is where a digest's blob lives. The digest is checked as a path
// component as much as a digest: it comes from manifests, which come from a
// registry.
func (l *Layout) blobPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	return filepath.Join(l.blobDir(), strings.TrimPrefix(digest, "sha256:")), nil
}

// hasBlob reports whether the layout holds d. Presence and size, not a hash: a
// blob only ever gets its name by being verified on the way in.
func (l *Layout) hasBlob(d Descriptor) bool {
	path, err := l.blobPath(d.Digest)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == d.Size
}

// readBlob reads a small blob — a manifest, an index, a config — and checks it
// against its digest, since what it says is then trusted to name the other
// blobs an image is made of.
func (l *Layout) readBlob(d Descriptor, limit int64) ([]byte, error) {
	path, err := l.blobPath(d.Digest)
	if err != nil {
		return nil, err
	}
	if d.Size > limit {
		return nil, fmt.Errorf("blob %s is %d bytes, more than the %d read here", d.Digest, d.Size, limit)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: blob %s is missing", ErrNotStaged, d.Digest)
	}
	if err != nil {
		return nil, err
	}
	if digestOf(data) != d.Digest {
		return nil, fmt.Errorf("blob %s does not match its digest", d.Digest)
	}
	return data, nil
}

// putBlob writes a blob whose bytes are already in hand, which is how the
// manifests arrive.
func (l *Layout) putBlob(d Descriptor, data []byte) error {
	if digestOf(data) != d.Digest || int64(len(data)) != d.Size {
		return fmt.Errorf("blob %s does not match its digest", d.Digest)
	}
	if l.hasBlob(d) {
		return nil
	}
	path, err := l.blobPath(d.Digest)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (l *Layout) readIndex() (imageIndex, error) {
	data, err := os.ReadFile(filepath.Join(l.dir, indexFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return imageIndex{SchemaVersion: 2, MediaType: mediaTypeOCIIndex}, nil
	}
	if err != nil {
		return imageIndex{}, err
	}
	var idx imageIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return imageIndex{}, fmt.Errorf("read %s: %w", filepath.Join(l.dir, indexFileName), err)
	}
	return idx, nil
}

// updateIndex applies change to the index while holding an exclusive lock on
// it, so two processes recording entries at once cannot lose one another's.
//
// index.json is the one file in the layout where meeting is unsafe: everything
// else is content-addressed and a second writer produces identical bytes, while
// here a read, an append and a rename can interleave into a dropped entry — an
// image that is complete on disk but not named, pulled again by a pool and
// deleted by the next prune.
//
// The lock is taken without blocking and retried briefly, because what it
// guards takes microseconds: a contended lock is a process about to release it.
// One that stays busy is reported rather than waited on forever; the entry is
// written again by the next staging, which finds every blob already present.
func (l *Layout) updateIndex(change func(*imageIndex)) error {
	lock, err := l.lockIndex()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	idx, err := l.readIndex()
	if err != nil {
		return err
	}
	change(&idx)
	return l.writeIndex(idx)
}

func (l *Layout) lockIndex() (*filelock.Lock, error) {
	path := filepath.Join(l.dir, indexLockName)
	deadline := time.Now().Add(indexLockTimeout)
	for {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s stayed locked by another process for %s", path, indexLockTimeout)
		}
		time.Sleep(indexLockPoll)
	}
}

const (
	// indexLockName is the lock guarding index.json, beside it.
	indexLockName = "index.lock"
	// indexLockTimeout and indexLockPoll bound waiting for that lock.
	indexLockTimeout = 10 * time.Second
	indexLockPoll    = 20 * time.Millisecond
)

// writeIndex replaces index.json as a whole, by rename, so a reader never sees
// half of one.
func (l *Layout) writeIndex(idx imageIndex) error {
	idx.SchemaVersion = 2
	idx.MediaType = mediaTypeOCIIndex
	if idx.Manifests == nil {
		idx.Manifests = []Descriptor{}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(l.dir, indexFileName), append(data, '\n'))
}

// writeFileAtomic writes a file beside its destination and renames it into
// place, so the name only ever holds a complete file.
func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), tempPrefix+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, writeErr := temp.Write(data)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// tempPrefix marks a file that is not yet what its name will be. Nothing reads
// one, and pruning removes one that is old enough to have been abandoned.
const tempPrefix = ".tmp-"

func findEntry(idx imageIndex, name string) (int, bool) {
	for i, entry := range idx.Manifests {
		if entry.Annotations[AnnotationImageName] == name {
			return i, true
		}
	}
	return -1, false
}

// Image is one reference, staged completely for one platform.
type Image struct {
	// Reference is what the image was staged under.
	Reference Reference
	// Top is the registry's own top-level manifest for the reference: an index
	// for a multi-platform image, and the image's manifest otherwise.
	Top Descriptor
	// Manifest, Config and Layers are the one platform's image beneath it.
	Manifest Descriptor
	Config   Descriptor
	Layers   []Descriptor

	layout *Layout
}

// Digest is the image's identity: the digest its registry serves the reference
// under, which is what a daemon reports in RepoDigests and what a sandbox is
// pinned to.
func (i *Image) Digest() string {
	return i.Top.Digest
}

// DigestReference names the image by its repository and digest, the form that
// asks a registry for exactly this image and no other.
func (i *Image) DigestReference() string {
	return i.Reference.qualifiedRepository() + "@" + i.Top.Digest
}

// Size is how many bytes of blobs an archive of the image carries.
func (i *Image) Size() int64 {
	total := i.Manifest.Size + i.Config.Size
	if i.Top.Digest != i.Manifest.Digest {
		total += i.Top.Size
	}
	for _, layer := range i.Layers {
		total += layer.Size
	}
	return total
}

// Lookup returns reference as staged for platform, or an error wrapping
// ErrNotStaged when the layout does not hold all of it.
func (l *Layout) Lookup(reference string, platform Platform) (*Image, error) {
	ref, err := ParseReference(reference)
	if err != nil {
		return nil, err
	}
	idx, err := l.readIndex()
	if err != nil {
		return nil, err
	}
	at, ok := findEntry(idx, ref.Name())
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotStaged, ref.Name())
	}
	return l.image(ref, idx.Manifests[at], platform)
}

// image assembles an Image beneath a top-level descriptor, requiring every blob
// the platform's image is made of.
func (l *Layout) image(ref Reference, top Descriptor, platform Platform) (*Image, error) {
	topData, err := l.readBlob(top, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref.Name(), err)
	}
	manifest, manifestData := top, topData
	if isIndex(top.MediaType) {
		if manifest, err = selectPlatform(topData, platform); err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Name(), err)
		}
		if manifestData, err = l.readBlob(manifest, maxManifestBytes); err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Name(), err)
		}
	}
	var m imageManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		return nil, fmt.Errorf("%s: read manifest: %w", ref.Name(), err)
	}
	for _, blob := range append([]Descriptor{m.Config}, m.Layers...) {
		if !l.hasBlob(blob) {
			return nil, fmt.Errorf("%w: %s is missing blob %s", ErrNotStaged, ref.Name(), blob.Digest)
		}
	}
	if !isIndex(top.MediaType) {
		// A single-platform image states its platform only in its config.
		if err := l.checkConfigPlatform(m.Config, platform); err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Name(), err)
		}
	}
	return &Image{
		Reference: ref,
		Top:       Descriptor{MediaType: top.MediaType, Digest: top.Digest, Size: top.Size},
		Manifest:  Descriptor{MediaType: manifest.MediaType, Digest: manifest.Digest, Size: manifest.Size},
		Config:    m.Config,
		Layers:    m.Layers,
		layout:    l,
	}, nil
}

// selectPlatform is an index's manifest for platform. Entries that are not
// images — the attestation manifests buildx publishes beside every platform,
// whose platform is unknown/unknown — never match.
func selectPlatform(indexData []byte, platform Platform) (Descriptor, error) {
	var idx imageIndex
	if err := json.Unmarshal(indexData, &idx); err != nil {
		return Descriptor{}, fmt.Errorf("read index: %w", err)
	}
	for _, entry := range idx.Manifests {
		if isManifest(entry.MediaType) && platform.matches(entry.Platform) {
			return entry, nil
		}
	}
	return Descriptor{}, fmt.Errorf("published with no %s image", platform)
}

// checkConfigPlatform reads an image config and checks it is for platform.
func (l *Layout) checkConfigPlatform(config Descriptor, platform Platform) error {
	data, err := l.readBlob(config, maxConfigBytes)
	if err != nil {
		return err
	}
	var got Platform
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	// Only a stated mismatch is refused. An image that declares no platform —
	// a hand-assembled artifact set — makes no claim to contradict.
	if (got.OS != "" && got.OS != platform.OS) || (got.Architecture != "" && got.Architecture != platform.Architecture) {
		return fmt.Errorf("the image is built for %s/%s, not %s", got.OS, got.Architecture, platform)
	}
	return nil
}

// Open reads one of the image's blobs, failing the read that would end it if
// its bytes are not the blob's digest — so a consumer reading an image back
// never acts on a blob that was damaged on disk after it was verified in.
func (i *Image) Open(d Descriptor) (io.ReadCloser, error) {
	path, err := i.layout.blobPath(d.Digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", i.Reference.Name(), err)
	}
	return &verifyingReader{file: file, hash: sha256.New(), want: d}, nil
}

// verifyingReader turns the end of a blob into an error when its bytes were
// not the ones its descriptor names.
type verifyingReader struct {
	file *os.File
	hash hash.Hash
	read int64
	want Descriptor
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	r.hash.Write(p[:n])
	r.read += int64(n)
	if errors.Is(err, io.EOF) {
		if got := "sha256:" + hex.EncodeToString(r.hash.Sum(nil)); r.read != r.want.Size || got != r.want.Digest {
			return n, fmt.Errorf("blob %s on disk has digest %s", r.want.Digest, got)
		}
	}
	return n, err
}

func (r *verifyingReader) Close() error {
	return r.file.Close()
}
