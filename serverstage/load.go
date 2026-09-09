package serverstage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// maxManifestBytes bounds what a manifest reference may deliver. A manifest is
// a few hundred bytes; anything past this is not one, and reading it into
// memory to discover that is how a wrong URL becomes a memory problem.
const maxManifestBytes = 1 << 20

// Load reads a manifest from a file path or an http(s) URL.
//
// This is the override: the manifest a build carries describes its own
// platform, and a caller staging for another one — a machine being provisioned,
// an image being built, a version other than this binary's — says so with a
// file. Nothing about the file is verified beyond being a valid manifest,
// because the caller named it; the digests inside it are still what the
// download is checked against.
func Load(ctx context.Context, ref string, client *http.Client) (Manifest, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Manifest{}, ErrNoManifest
	}
	if parsed, err := url.Parse(ref); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return fetchManifest(ctx, ref, client)
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("read server manifest: %w", err)
	}
	return ParseManifest(data)
}

func fetchManifest(ctx context.Context, ref string, client *http.Client) (Manifest, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return Manifest{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch server manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("fetch server manifest: %s: %s", ref, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch server manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("fetch server manifest: %s is larger than %d bytes, which no manifest is", ref, maxManifestBytes)
	}
	return ParseManifest(data)
}
