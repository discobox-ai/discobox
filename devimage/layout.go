package devimage

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/imagecache"
)

// maxLayoutIndex bounds the index.json read from a layout: it lists one
// manifest.
const maxLayoutIndex = 1 << 20

// LayoutDigest is the digest of the one manifest an OCI image layout tar's
// index.json lists: what a bundle the watcher exports (`docker buildx build
// --output type=oci`) is pinned by. A layout listing anything but one image
// manifest is refused, since it is not one bundle's.
func LayoutDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open bundle layout %s: %w", path, err)
	}
	defer file.Close()
	entries := tar.NewReader(file)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("bundle layout %s has no index.json", path)
		}
		if err != nil {
			return "", fmt.Errorf("read bundle layout %s: %w", path, err)
		}
		if layoutName(header.Name) != "index.json" {
			continue
		}
		var index struct {
			Manifests []struct {
				MediaType string `json:"mediaType"`
				Digest    string `json:"digest"`
			} `json:"manifests"`
		}
		if err := json.NewDecoder(io.LimitReader(entries, maxLayoutIndex)).Decode(&index); err != nil {
			return "", fmt.Errorf("decode bundle layout %s index.json: %w", path, err)
		}
		if len(index.Manifests) != 1 {
			return "", fmt.Errorf("bundle layout %s lists %d manifests, not one bundle's", path, len(index.Manifests))
		}
		entry := index.Manifests[0]
		if entry.MediaType != imagecache.MediaTypeOCIManifest && entry.MediaType != imagecache.MediaTypeDockerManifest {
			return "", fmt.Errorf("bundle layout %s lists a %s, not an image manifest", path, entry.MediaType)
		}
		if !imagecache.IsDigest(entry.Digest) {
			return "", fmt.Errorf("bundle layout %s lists the manifest digest %q", path, entry.Digest)
		}
		return entry.Digest, nil
	}
}

// WalkLayoutBlobs calls visit with every blob an OCI image layout tar holds,
// by the digest its name claims; the bytes are the tar's, and checking them
// against that claim is visit's.
func WalkLayoutBlobs(path string, visit func(digest string, blob io.Reader) error) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open bundle layout %s: %w", path, err)
	}
	defer file.Close()
	entries := tar.NewReader(file)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read bundle layout %s: %w", path, err)
		}
		digest, ok := layoutBlobDigest(header)
		if !ok {
			continue
		}
		if err := visit(digest, entries); err != nil {
			return err
		}
	}
}

// OpenLayoutBlob reads the one blob named digest from an OCI image layout
// tar, unchecked as WalkLayoutBlobs's are. A layout not holding it is an
// error wrapping os.ErrNotExist.
func OpenLayoutBlob(path, digest string) (io.ReadCloser, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle layout %s: %w", path, err)
	}
	entries := tar.NewReader(file)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			_ = file.Close()
			return nil, fmt.Errorf("bundle layout %s holds no blob %s: %w", path, digest, os.ErrNotExist)
		}
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read bundle layout %s: %w", path, err)
		}
		if named, ok := layoutBlobDigest(header); ok && named == digest {
			return layoutBlob{Reader: entries, file: file}, nil
		}
	}
}

type layoutBlob struct {
	io.Reader
	file *os.File
}

func (b layoutBlob) Close() error { return b.file.Close() }

// layoutBlobDigest is the digest a layout tar entry's name claims, for a
// regular file under blobs/sha256/.
func layoutBlobDigest(header *tar.Header) (string, bool) {
	if header.Typeflag != tar.TypeReg {
		return "", false
	}
	hex, ok := strings.CutPrefix(layoutName(header.Name), "blobs/sha256/")
	if !ok || !imagecache.IsDigest("sha256:"+hex) {
		return "", false
	}
	return "sha256:" + hex, true
}

func layoutName(name string) string {
	return strings.TrimPrefix(name, "./")
}

// Check reports whether bundle's layout still holds the bundle it records: a
// layout whose index lists another manifest than Digest is not this bundle,
// and nothing pinned by Digest is answered from it.
func (b Bundle) Check() error {
	digest, err := LayoutDigest(b.Layout)
	if err != nil {
		return err
	}
	if digest != b.Digest {
		return fmt.Errorf("bundle layout %s holds %s, not the %s it was published as", b.Layout, digest, b.Digest)
	}
	return nil
}
