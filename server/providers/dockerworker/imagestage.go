package dockerworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// StageImages makes every image in images present on the pool's daemon.
//
// The images a sandbox runs are normally pulled by the pool agent at the moment
// a sandbox wants one, which is the moment a user is watching. Pulling them
// here instead moves that cost to server startup, where it is paid once and in
// the open. It is the engine's job rather than the agent's for the same reason
// the pool image is: the engine owns what is on a pool daemon, and it can reach
// that daemon before there is an agent on it.
//
// Failures are collected rather than returned on the first one. A harness image
// that no longer exists in the registry should not stop the other four from
// being ready, and none of this is required for the server to work — it is an
// optimisation for the wait that would otherwise happen later.
func (e *Engine) StageImages(ctx context.Context, pool *model.Pool, images []string, report func(sandbox.PreloadProgress)) error {
	if pool == nil || len(images) == 0 {
		return nil
	}
	lease, err := e.acquireDockerReady(ctx, pool.ID)
	if err != nil {
		return fmt.Errorf("reach the pool's Docker daemon: %w", err)
	}
	defer lease.Release()

	var failures []error
	for i, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		if report != nil {
			report(sandbox.PreloadProgress{Image: image, Done: i, Total: len(images)})
		}
		started := time.Now()
		onPull := func(pull sandbox.PoolPullProgress) {
			if report != nil {
				report(sandbox.PreloadProgress{Image: image, Done: i, Total: len(images), Pull: &pull})
			}
		}
		if err := e.ensureImageRef(ctx, lease.Client, pool.ID, image, sandbox.PoolPhasePreloadingImages, onPull); err != nil {
			// A canceled preload is the server shutting down, not an image
			// problem, and reporting every remaining image as broken would bury
			// that.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.WarnContext(ctx, "preload image failed", "pool", pool.ID, "image", image, "error", err)
			failures = append(failures, err)
			continue
		}
		slog.InfoContext(ctx, "preloaded image", "pool", pool.ID, "image", image, "duration", time.Since(started))
	}
	if report != nil {
		report(sandbox.PreloadProgress{Done: len(images), Total: len(images)})
	}
	return errors.Join(failures...)
}
