package assets

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/imagecache"
)

// Removal is by reference (ADR 26-10-16-423 §13): a blob leaves the store
// when no pin — a sandbox or harness config of any project, or a platform
// digest this server has answered — reaches it any more.
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
	sandboxes, err := s.store.ListSandboxBundles(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("prune skipped: list sandbox bundles: %w", err)
	}
	keep := map[string]bool{}
	for _, sb := range sandboxes {
		for _, digest := range []string{sb.BundleDigest, sb.PlatformBundleDigest} {
			if imagecache.IsDigest(digest) {
				s.blobs.Reach(digest, keep)
			}
		}
	}
	for _, digest := range platform {
		s.blobs.Reach(digest, keep)
	}
	for _, config := range configs {
		if imagecache.IsDigest(config.BundleDigest) {
			s.blobs.Reach(config.BundleDigest, keep)
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
