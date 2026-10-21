package assets

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Removal is by reference (ADR 26-10-16-423 §13): a blob leaves the store
// when no pin — a harness config of any project, or a platform digest this
// server has answered — reaches it any more.
const (
	// pruneInterval is how often the store is swept.
	pruneInterval = time.Hour
	// pruneGrace is how old an unreached blob must be before it goes, so that
	// one fetched for a pin made since the sweep read the pins is kept.
	pruneGrace = time.Hour
)

// StartPruning sweeps the store every pruneInterval until ctx ends.
func (s *Service) StartPruning(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if removed, err := s.Prune(ctx); err != nil {
					slog.Warn("asset store prune", "error", err)
				} else if removed > 0 {
					slog.Info("asset store prune", "removed", removed)
				}
			}
		}
	}()
}

// Prune removes every blob no pin reaches, and reports how many it removed.
//
// What a pin reaches is read from the store alone, never fetched: a manifest
// the store lacks has nothing beneath it there to keep. When the platform the
// release pins cannot be resolved, nothing is removed, since what it reaches
// is then unknown.
func (s *Service) Prune(ctx context.Context) (int, error) {
	platform, err := s.platform.pins(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("prune skipped: %w", err)
	}
	configs, err := s.store.ListHarnessConfigBundles(ctx)
	if err != nil {
		return 0, fmt.Errorf("prune skipped: list harness config bundles: %w", err)
	}
	keep := map[string]bool{}
	for _, digest := range platform {
		s.reach(digest, keep)
	}
	for _, config := range configs {
		if digestPattern.MatchString(config.BundleDigest) {
			s.reach(config.BundleDigest, keep)
		}
	}
	removed, err := s.blobs.PruneBlobs(func(digest string) bool { return keep[digest] }, s.now().Add(-pruneGrace))
	for key := range s.manifests.Range {
		if digest, ok := key.(string); ok && !keep[digest] {
			s.manifests.Delete(key)
		}
	}
	return removed, err
}

// reach marks digest and everything beneath it the store holds.
func (s *Service) reach(digest string, keep map[string]bool) {
	if keep[digest] {
		return
	}
	keep[digest] = true
	read, ok := s.storedManifest(digest)
	if !ok {
		return
	}
	for _, child := range read.Manifests {
		s.reach(child.Digest, keep)
	}
	if read.Config != nil {
		keep[read.Config.Digest] = true
	}
	for _, layer := range read.Layers {
		keep[layer.Digest] = true
	}
}

// storedManifest reads a manifest the store already holds, fetching nothing.
func (s *Service) storedManifest(digest string) (manifest, bool) {
	if cached, ok := s.manifests.Load(digest); ok {
		if read, ok := cached.(manifest); ok {
			return read, true
		}
	}
	body, size, err := s.blobs.OpenBlob(digest)
	if err != nil {
		return manifest{}, false
	}
	defer body.Close()
	if size > maxManifestBytes {
		return manifest{}, false
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return manifest{}, false
	}
	read, err := parseManifest(data)
	return read, err == nil
}
