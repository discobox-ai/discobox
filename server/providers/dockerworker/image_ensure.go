package dockerworker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ensureImage makes the pool-agent image present on cli's daemon, pulling it if
// it is not.
//
// Docker does not pull on ContainerCreate: an absent image is a plain "No such
// image" error, not a fetch. Nothing else fills the gap for this image either.
// DevelopmentImageSync side-loads it from the developer's own daemon, which is
// exactly what a development build wants and exactly what a released binary
// does not have — it is nil there, so a release reached ContainerCreate with
// the pool daemon holding no such image and no way to get one. The sandbox
// image was never affected because the pool agent pulls that itself
// (sandboxruntime.ensureImageAvailable); this is the same rule applied one
// level up, to the image that pool agent ships in.
//
// Inspect first rather than pulling unconditionally: a development image tag
// exists on no registry, so a pull would fail on the very images that are
// already correctly in place.
func (e *Engine) ensureImage(ctx context.Context, cli *client.Client) error {
	image := e.cfg.Image
	if _, err := cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect pool image %q: %w", image, err)
	}

	logger := slog.Default()
	logger.Info("pulling pool image", "image", image)
	started := time.Now()
	pull, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull pool image %q: %w", image, err)
	}
	defer pull.Close()
	// Wait drains the daemon's progress stream, and draining is what actually
	// runs the pull.
	if err := pull.Wait(ctx); err != nil {
		return fmt.Errorf("pull pool image %q: %w", image, err)
	}
	logger.Info("pulled pool image", "image", image, "duration", time.Since(started))
	return nil
}
