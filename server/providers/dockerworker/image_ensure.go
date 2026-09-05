package dockerworker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
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
//
// It reports the reference to launch, which is the configured image whenever
// that image can be had and a superseded one already on the daemon when it
// cannot — see fallbackPoolImage.
func (e *Engine) ensureImage(ctx context.Context, cli *client.Client, poolID string) (string, error) {
	err := e.ensureImageRef(ctx, cli, poolID, e.cfg.Image, sandbox.PoolPhasePullingPoolImage, nil)
	if err == nil {
		return e.cfg.Image, nil
	}
	fallback, ok := e.fallbackPoolImage(ctx, cli)
	if !ok {
		return "", err
	}
	slog.WarnContext(ctx, "launching a superseded pool agent: the configured image could not be pulled",
		"pool", poolID, "configured_image", e.cfg.Image, "image", fallback, "error", err)
	return fallback, nil
}

// fallbackPoolImage is a pool-agent image already on cli's daemon to launch
// when the configured one cannot be had.
//
// A pool whose agent is a release behind still schedules sandboxes; a pool with
// no agent at all schedules nothing. A registry that is briefly unreachable —
// the guest's own boot race against DHCP is one way to get there — is not worth
// taking a working pool offline over, so a superseded image that is already
// local beats failing the reconcile.
//
// Only the configured image's own repository is considered: the pool agent is
// the one thing this may launch, and no other image on the daemon substitutes
// for it. The most recently built tag wins, which is the newest release the
// daemon has ever held. Build time rather than local arrival time, because it
// orders releases the way releases are ordered and costs no per-image inspect
// to read.
//
// This is a fallback, never a resting place: the caller compares the running
// container against a freshly resolved reference on every reconcile, so the
// pool upgrades itself as soon as the configured image can be pulled.
func (e *Engine) fallbackPoolImage(ctx context.Context, cli *client.Client) (string, bool) {
	repository, err := imageRepository(e.cfg.Image)
	if err != nil {
		return "", false
	}
	filters := client.Filters{}
	filters = filters.Add("reference", repository)
	images, err := cli.ImageList(ctx, client.ImageListOptions{Filters: filters})
	if err != nil {
		slog.WarnContext(ctx, "list local pool images for a fallback", "repository", repository, "error", err)
		return "", false
	}
	best, bestCreated := "", int64(0)
	for _, image := range images.Items {
		for _, tag := range image.RepoTags {
			// The daemon-side filter is a pattern match; this is what makes the
			// repository exact, and what carries the check on a daemon whose
			// filter matched more broadly.
			if got, err := imageRepository(tag); err != nil || got != repository {
				continue
			}
			// The configured image is what could not be had. If it is somehow
			// listed, it is not the answer to its own absence.
			if tag == e.cfg.Image {
				continue
			}
			// Tag order breaks a tie between two tags of one image, so the
			// choice does not depend on the order the daemon lists them in.
			if best == "" || image.Created > bestCreated || (image.Created == bestCreated && tag > best) {
				best, bestCreated = tag, image.Created
			}
		}
	}
	return best, best != ""
}

// imageRepository is an image reference's repository, normalized so that a
// short local reference and a fully qualified one for the same repository
// compare equal.
func imageRepository(image string) (string, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return "", err
	}
	return named.Name(), nil
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
