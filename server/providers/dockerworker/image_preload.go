package dockerworker

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/discobox-ai/discobox/imagecache"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/moby/moby/client"
)

// preloadImages loads only images already staged on this machine. Missing
// images are left for the sandbox's on-demand pull; there is no prepull loop.
func (e *Engine) preloadImages(ctx context.Context, lease *DockerClientLease, poolID string, images []string) error {
	if len(images) == 0 || e.cfg.ImageCache == nil {
		return nil
	}
	if lease.Locality != DaemonOnThisMachine {
		return nil
	}
	info, err := lease.Client.Info(ctx, client.InfoOptions{})
	if err != nil {
		return err
	}
	platform, ok := daemonPlatform(info.Info)
	if !ok {
		return nil
	}
	for _, image := range images {
		if _, err := lease.Client.ImageInspect(ctx, image); err == nil {
			continue
		} else if !cerrdefs.IsNotFound(err) {
			return err
		}
		cached, err := e.cfg.ImageCache.Lookup(image, platform)
		if errors.Is(err, imagecache.ErrNotStaged) {
			continue
		} else if err != nil {
			return err
		}
		err = e.importCachedImage(ctx, lease.Client, poolID, image, cached, usesContainerdStore(info.Info), sandbox.PoolPhasePreloadingImages)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return fmt.Errorf("preload image %q: %w", image, err)
		}
	}
	return nil
}
