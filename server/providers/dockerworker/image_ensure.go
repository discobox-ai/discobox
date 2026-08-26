package dockerworker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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
func (e *Engine) ensureImage(ctx context.Context, cli *client.Client, poolID string) error {
	return e.ensureImageRef(ctx, cli, poolID, e.cfg.Image, sandbox.PoolPhasePullingPoolImage, nil)
}

// ensureImageRef is ensureImage for any image on this pool's daemon, reporting
// under the phase the caller is in.
// onPull, when set, is called with the running totals as they move, for a
// caller that is narrating this pull rather than merely recording it.
func (e *Engine) ensureImageRef(ctx context.Context, cli *client.Client, poolID, image string, phase sandbox.PoolProvisionPhase, onPull func(sandbox.PoolPullProgress)) error {
	if _, err := cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect pool image %q: %w", image, err)
	}

	logger := slog.Default()
	logger.Info("pulling image", "image", image, "pool", poolID)
	started := time.Now()
	pull, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %q: %w", image, err)
	}
	defer pull.Close()
	// The stream is read rather than waited on, because draining it is what
	// runs the pull either way and this is the one phase of bringing a pool up
	// that can say how far in it is. A sandbox waiting for a pool to take it
	// spends most of its wait right here on a cold host.
	if err := e.consumePoolImagePull(ctx, pull, poolID, image, phase, onPull); err != nil {
		return fmt.Errorf("pull image %q: %w", image, err)
	}
	logger.Info("pulled image", "image", image, "pool", poolID, "duration", time.Since(started))
	return nil
}

// poolPullReportInterval is how often a pull in flight is written to the pool
// row. Twice a second is what the sandbox-side pull reports at, and it is the
// rate a byte counter has to move at to read as movement rather than as a
// series of jumps.
const poolPullReportInterval = 500 * time.Millisecond

// consumePoolImagePull drains a pull's progress stream, reporting as it goes.
//
// Draining is mandatory — it is what advances the pull — so the reporting is
// free; what it must not do is report every message, of which there are
// thousands.
func (e *Engine) consumePoolImagePull(ctx context.Context, pull client.ImagePullResponse, poolID, image string, phase sandbox.PoolProvisionPhase, onPull func(sandbox.PoolPullProgress)) error {
	layers := map[string]struct {
		current int64
		total   int64
		done    bool
	}{}
	var lastReport time.Time
	report := func(done bool) {
		progress := sandbox.PoolPullProgress{Image: image, Done: done}
		for _, layer := range layers {
			progress.Current += layer.current
			progress.Total += layer.total
			progress.Layers++
			if layer.done {
				progress.LayersComplete++
			}
		}
		e.cfg.ProgressReporter.ReportProgress(ctx, poolID, sandbox.PoolProvisionProgress{
			Phase: phase,
			Pull:  &progress,
		})
		if onPull != nil {
			onPull(progress)
		}
	}
	// Once before the first byte, so the phase is on the row even for a pull
	// that turns out to be entirely cached.
	report(false)
	for message, err := range pull.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if message.Error != nil {
			return message.Error
		}
		if message.ID != "" {
			layer := layers[message.ID]
			// "Pull complete" and "Already exists" both end a layer, and a
			// layer that was already present reports no bytes at all — which is
			// why layers complete is counted rather than derived from bytes.
			// Only the download phase's byte counts are accumulated.
			//
			// Docker reuses progressDetail for extraction, restarting current
			// from zero against the uncompressed size — so taking every report
			// at face value made the running total fall as layers finished
			// downloading, and a byte counter that goes backwards is worse than
			// no byte counter. A finished layer is pinned at its own total, and
			// its bytes stop moving.
			switch message.Status {
			case "Pull complete", "Already exists":
				layer.done = true
				layer.current = layer.total
			case "Downloading":
				if message.Progress != nil {
					if message.Progress.Total > 0 {
						layer.total = message.Progress.Total
					}
					// Never down: a retried layer restarts its count, and the
					// pull as a whole has not un-downloaded anything.
					if message.Progress.Current > layer.current {
						layer.current = message.Progress.Current
					}
				}
			case "Download complete", "Verifying Checksum", "Extracting":
				// The bytes are in. What follows is local work with a
				// denominator of its own, which this line does not report.
				layer.current = layer.total
			}
			layers[message.ID] = layer
		}
		if time.Since(lastReport) >= poolPullReportInterval {
			lastReport = time.Now()
			report(false)
		}
	}
	report(true)
	return nil
}
