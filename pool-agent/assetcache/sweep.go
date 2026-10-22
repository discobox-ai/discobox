package assetcache

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Removal is by reference (ADR 26-10-16-423 §13): a blob leaves the store when
// no sandbox's record reaches it any more.
const (
	// sweepInterval is how often the store is swept.
	sweepInterval = time.Hour
	// sweepGrace is how old an unreached blob must be before it goes, so that
	// one fetched for a create recorded since the sweep read the records stays.
	sweepGrace = time.Hour
)

// StartSweeping sweeps the store every sweepInterval until ctx ends.
func (c *Cache) StartSweeping(ctx context.Context, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if removed, err := c.Sweep(); err != nil {
					logger.Warn("asset cache sweep", "error", err)
				} else if removed > 0 {
					logger.Info("asset cache sweep", "removed", removed)
				}
			}
		}
	}()
}

// Sweep removes every blob no sandbox's record reaches, and reports how many
// it removed. What a pin reaches is read from the store alone, never fetched:
// a manifest the store lacks has nothing beneath it there to keep. A record
// that cannot be read removes nothing, since what it reaches is then unknown.
func (c *Cache) Sweep() (int, error) {
	pins, err := allPins(c.sandboxes)
	if err != nil {
		return 0, fmt.Errorf("sweep skipped: %w", err)
	}
	keep := map[string]bool{}
	for _, pin := range pins {
		c.blobs.Reach(pin.Digest, keep)
	}
	removed, err := c.blobs.PruneBlobs(func(digest string) bool { return keep[digest] }, c.now().Add(-sweepGrace))
	for key := range c.manifests.Range {
		if digest, ok := key.(string); ok && !keep[digest] {
			c.manifests.Delete(key)
		}
	}
	return removed, err
}
