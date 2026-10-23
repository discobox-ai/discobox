package dockerworker

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/moby/moby/client"
)

// preloadImage loads the sandbox base image, every sandbox container's, when
// it is already staged on this machine. One that is not is left for the
// sandbox's on-demand pull; there is no prepull loop. No harness is an image
// (ADR 26-10-16-423 §10), so it is the one image a sandbox runs.
func (e *Engine) preloadImage(ctx context.Context, lease *DockerClientLease, poolID, image string) error {
	if image == "" || e.cfg.ImageCache == nil {
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
	if _, err := lease.Client.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	cached, err := e.cfg.ImageCache.Lookup(image, platform)
	if errors.Is(err, imagecache.ErrNotStaged) {
		return nil
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
	return nil
}
