package dockerworker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/pool-agent/imagereap"
)

// ReclaimImages removes the Discobox images on cli that nothing uses and that
// have outlived the retention window (ADR 0040).
//
// It lives here for the same reason image sync does: a driver provides
// connectivity, and the engine owns what is on the daemon behind it. That makes
// this the one place that knows both the pool image it launches and the
// development images it loaded, which together are what must survive
// reclamation without a container to prove they are needed.
func (e *Engine) ReclaimImages(ctx context.Context, cli *client.Client, logger *slog.Logger) error {
	if e == nil {
		return nil
	}
	_, err := imagereap.Reclaim(ctx, cli, imagereap.Options{
		Retention: e.imageRetention(),
		Keep:      e.imageKeepReferences(),
		Logger:    logger,
	})
	return err
}

// imageRetention is how long this engine's daemons keep an unused image.
func (e *Engine) imageRetention() time.Duration {
	if e.cfg.ImageRetention > 0 {
		return e.cfg.ImageRetention
	}
	return imagereap.DefaultRetention
}

// ImageReclaimInterval is how often to reclaim on a daemon hosting this
// engine's pool containers. It follows the retention window, so a development
// daemon — which supersedes an image every few minutes — is swept on a matching
// cadence rather than hourly.
func (e *Engine) ImageReclaimInterval() time.Duration {
	return imagereap.ReclaimInterval(e.imageRetention())
}

// imageReclaimTimeout bounds a pass driven by a pool reconcile, which the pass
// would otherwise hold up if the daemon were slow to delete.
const imageReclaimTimeout = 2 * time.Minute

// reclaimImagesForPool reclaims images on the daemon behind one pool, at most
// once per ImageReclaimInterval for that pool.
//
// This is what makes reclamation cover every backend rather than only the local
// Docker provider. A VM backend has no long-lived host Docker client for a
// standing loop to hold — the daemon exists only once the pool's VM does, and it
// is reached through a per-reconcile lease. Every backend does pass through
// EnsurePool, so hanging reclamation there covers all of them.
//
// The cadence suits what actually accumulates on such a daemon: one superseded
// pool image per upgrade. An upgrade is itself a pool reconcile, so each upgrade
// reclaims the image the previous one left behind.
//
// A failure is logged, never returned. Reclaiming disk must not fail a pool
// reconcile.
func (e *Engine) reclaimImagesForPool(ctx context.Context, cli *client.Client, poolID string) {
	if e == nil || !e.imageReclaim.claim(poolID, time.Now(), e.ImageReclaimInterval()) {
		return
	}
	// Bounded because this runs inside the reconcile, holding the pool's Docker
	// lease. Reclaiming disk is worth a moment and never worth stalling a pool;
	// a pass cut short here simply resumes at the next reconcile.
	ctx, cancel := context.WithTimeout(ctx, imageReclaimTimeout)
	defer cancel()
	if err := e.ReclaimImages(ctx, cli, slog.Default()); err != nil {
		slog.WarnContext(ctx, "reclaim unused Discobox images", "pool_id", poolID, "error", err)
	}
}

// imageReclaimThrottle records the last reclamation per pool. Pool ID stands in
// for daemon identity: it is exactly one-per-daemon on every VM backend, and on
// a shared local daemon it only over-approximates, costing a redundant pass that
// the standing local loop would have run anyway.
type imageReclaimThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// claim reports whether a pass may run now, recording it when it may.
func (t *imageReclaimThrottle) claim(poolID string, now time.Time, interval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[poolID]; ok && now.Sub(last) < interval {
		return false
	}
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	t.last[poolID] = now
	return true
}

// imageKeepReferences names the images this engine needs present whether or not
// a container currently runs one: the pool image it launches, which has no
// container while every pool is stopped, and the development image set.
func (e *Engine) imageKeepReferences() []string {
	keep := make([]string, 0, 8)
	if image := e.cfg.Image; image != "" {
		keep = append(keep, image)
	}
	return append(keep, e.cfg.DevelopmentImageSync.KeepReferences()...)
}
