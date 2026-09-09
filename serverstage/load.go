package serverstage

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Load reads a manifest from a file.
//
// This is the override: the manifest a build carries describes its own
// platform, and a caller staging for another one — a machine being
// provisioned, an image being built, a version other than this binary's — says
// so with a file.
//
// A file and nothing else. Fetching a manifest over the network is the one
// capability ADR 0099 §3 rejected and §8 deferred: the digests in it are what
// every download is checked against, so a manifest that arrived over TLS and
// nothing else moves the trust root from the binary to whoever answered the
// request. A caller who wants a published manifest downloads it themselves,
// which is where that decision belongs.
//
// The context is taken because reading a manifest is a step in a cancellable
// operation and every other step in it takes one; a caller should not have to
// know which steps happen to touch the network.
func Load(ctx context.Context, ref string) (Manifest, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Manifest{}, ErrNoManifest
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if scheme, _, ok := strings.Cut(ref, "://"); ok {
		return Manifest{}, fmt.Errorf("server manifest %q is a %s URL; this takes a file, so download it first", ref, scheme)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("read server manifest: %w", err)
	}
	return ParseManifest(data)
}
