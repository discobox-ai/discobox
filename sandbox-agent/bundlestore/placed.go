package bundlestore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// PlacedFetcher reads what a pool's maker placed in the box under Root
// (agentlayer.PlacedBlob), fetching nothing (ADR 26-10-30-875 §5): each
// manifest and blob by its digest, whatever name a manifest is asked under.
// What it reads is checked against its digest by the store, as a fetch is.
type PlacedFetcher struct {
	Root string
}

// Fetch reads path, /<name>/manifests/<digest> or /blobs/<digest>, from
// what was placed. A digest nothing was placed for is an error.
func (f PlacedFetcher) Fetch(_ context.Context, path string) (io.ReadCloser, error) {
	digest := path[strings.LastIndex(path, "/")+1:]
	parent := strings.TrimSuffix(path, "/"+digest)
	if !imagecache.IsDigest(digest) || (parent != "/blobs" && !strings.HasSuffix(parent, "/manifests")) {
		return nil, fmt.Errorf("read %s: not a manifest or blob by digest", path)
	}
	body, err := os.Open(filepath.Join(f.Root, filepath.FromSlash(agentlayer.PlacedBlob(digest))))
	if err != nil {
		return nil, fmt.Errorf("read %s, which the pool's maker placed: %w", digest, err)
	}
	return body, nil
}
