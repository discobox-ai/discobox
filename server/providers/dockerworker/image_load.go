package dockerworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/imagecache"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// Loading an image from the image cache on this machine instead of pulling it
// (ADR 0113).
//
// The CLI that launched this server staged the release's images into an OCI
// layout before starting it, so the bytes a pool would pull are already on this
// machine's disk. A load is still work — the daemon extracts every layer — but
// none of it waits on a network, and a second pool loads the same bytes rather
// than downloading them again.
//
// The image has to arrive under the identity a pull would have given it,
// because that identity is what a harness config pins a sandbox to (ADR 0016
// §6) and what the pool agent checks RepoDigests against. The two image stores
// get there differently:
//
//   - The containerd store imports the archive's index.json as the registry's
//     own index, so the loaded image's ID and RepoDigests are the pinned digest
//     as it lands.
//   - The classic store reads manifest.json and records no registry digest. A
//     pull by digest then finds the image's config already present, downloads
//     no layer, and records the digest — the same identity, for the cost of two
//     manifests. That is moby's own shortcut, checked against Docker 26.1.5 (the
//     guest image's) and 29.
//
// Every failure leaves the daemon without the image and answers false, and the
// caller pulls, exactly as it did before any of this existed.

// containerdSnapshotter is the driver-type a daemon on the containerd image
// store reports among its storage driver's status.
const containerdSnapshotter = "io.containerd.snapshotter.v1"

// loadFromCache loads image into cli's daemon from the image cache, reporting
// under phase as it goes, and says whether it did.
func (e *Engine) loadFromCache(ctx context.Context, lease *DockerClientLease, poolID, image string, phase sandbox.PoolProvisionPhase, onProgress imageProgress) bool {
	// A daemon on another machine is closer to its registry than to this
	// machine's disk, and only the driver that handed over this client knows
	// which it is.
	if e.cfg.ImageCache == nil || lease.Locality != DaemonOnThisMachine {
		return false
	}
	cli := lease.Client
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		slog.WarnContext(ctx, "image cache: could not ask the pool's daemon what it runs", "pool", poolID, "image", image, "error", err)
		return false
	}
	platform, ok := daemonPlatform(info.Info)
	if !ok {
		return false
	}
	cached, err := e.cfg.ImageCache.Lookup(image, platform)
	if err != nil {
		if !errors.Is(err, imagecache.ErrNotStaged) {
			slog.WarnContext(ctx, "image cache: lookup failed", "pool", poolID, "image", image, "error", err)
		}
		return false
	}
	started := time.Now()
	if err := e.loadCached(ctx, cli, poolID, cached, phase, onProgress); err != nil {
		slog.WarnContext(ctx, "image cache: load failed; pulling instead", "pool", poolID, "image", image, "error", err)
		removeLoaded(ctx, cli, image)
		return false
	}
	if !usesContainerdStore(info.Info) {
		if err := recordRegistryDigest(ctx, cli, cached); err != nil {
			// A loaded image the pin can never match is worse than an absent
			// one: present, it is never pulled over.
			slog.WarnContext(ctx, "image cache: could not record the loaded image's registry digest; pulling instead", "pool", poolID, "image", image, "error", err)
			removeLoaded(ctx, cli, image)
			return false
		}
	}
	if err := verifyLoaded(ctx, cli, image, cached.Digest()); err != nil {
		slog.WarnContext(ctx, "image cache: the loaded image is not the one staged; pulling instead", "pool", poolID, "image", image, "error", err)
		removeLoaded(ctx, cli, image)
		return false
	}
	slog.InfoContext(ctx, "loaded image from the image cache", "pool", poolID, "image", image,
		"digest", cached.Digest(), "bytes", cached.Size(), "duration", time.Since(started))
	return true
}

// daemonPlatform is the platform a daemon runs images for, in the terms an
// image index uses. Docker reports the kernel's name for the architecture.
func daemonPlatform(info system.Info) (imagecache.Platform, bool) {
	architecture := map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
	}[strings.ToLower(info.Architecture)]
	if architecture == "" || info.OSType == "" {
		return imagecache.Platform{}, false
	}
	return imagecache.Platform{OS: info.OSType, Architecture: architecture}, true
}

func usesContainerdStore(info system.Info) bool {
	for _, status := range info.DriverStatus {
		if status[0] == "driver-type" && status[1] == containerdSnapshotter {
			return true
		}
	}
	return false
}

// loadCached streams the image's archive into the daemon, reporting the bytes
// written as a pull reports the bytes downloaded.
func (e *Engine) loadCached(ctx context.Context, cli *client.Client, poolID string, cached *imagecache.Image, phase sandbox.PoolProvisionPhase, onProgress imageProgress) error {
	progress := sandbox.PoolPullProgress{Image: cached.Reference.Name(), Total: cached.Size(), Layers: len(cached.Layers)}
	report := func() {
		e.cfg.ProgressReporter.ReportProgress(ctx, poolID, sandbox.PoolProvisionProgress{Phase: phase, Pull: &progress})
		if onProgress != nil {
			onProgress(progress, true)
		}
	}
	// Once before the first byte, so the phase is on the row from the start.
	report()

	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() {
		// progress is this goroutine's alone until written is received.
		var lastReport time.Time
		err := cached.WriteArchive(writer, func(current int64, layersComplete int) {
			progress.Current, progress.LayersComplete = current, layersComplete
			if time.Since(lastReport) >= poolPullReportInterval {
				lastReport = time.Now()
				report()
			}
		})
		_ = writer.CloseWithError(err)
		written <- err
	}()
	load, err := cli.ImageLoad(ctx, reader, client.ImageLoadWithQuiet(true))
	if err != nil {
		_ = reader.CloseWithError(err)
		return errors.Join(err, <-written)
	}
	loadErr := readLoadResponse(load)
	_ = load.Close()
	// Closed before waiting, so an archive the daemon stopped reading ends
	// rather than blocking on a pipe nothing drains.
	_ = reader.Close()
	if err := errors.Join(<-written, loadErr); err != nil {
		return err
	}
	progress.Done = true
	report()
	return nil
}

// readLoadResponse drains a load's response, which reports failure inside the
// stream rather than in its status code.
func readLoadResponse(body io.Reader) error {
	decoder := json.NewDecoder(body)
	for {
		var message struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read load response: %w", err)
		}
		if message.ErrorDetail != nil && message.ErrorDetail.Message != "" {
			return errors.New(message.ErrorDetail.Message)
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
	}
}

// recordRegistryDigest gives an image a classic daemon has just loaded the
// registry digest a pull would have recorded, by pulling it by that digest.
// Its config is already present, so the puller returns before it fetches a
// layer; what crosses the network is the index and one manifest.
func recordRegistryDigest(ctx context.Context, cli *client.Client, cached *imagecache.Image) error {
	pull, err := cli.ImagePull(ctx, cached.DigestReference(), client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer pull.Close()
	for message, err := range pull.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if message.Error != nil {
			return message.Error
		}
	}
	return nil
}

// verifyLoaded checks the daemon now knows image by the digest it was staged
// under — the property everything above exists to produce, checked rather
// than assumed of either store.
func verifyLoaded(ctx context.Context, cli *client.Client, image, digest string) error {
	inspected, err := cli.ImageInspect(ctx, image)
	if err != nil {
		return err
	}
	if inspected.ID == digest {
		return nil
	}
	for _, repoDigest := range inspected.RepoDigests {
		if _, got, ok := strings.Cut(repoDigest, "@"); ok && got == digest {
			return nil
		}
	}
	return fmt.Errorf("the daemon knows it as %s, not %s", inspected.ID, digest)
}

// removeLoaded takes back what a failed load left, so the pull that follows
// is not answered by an image the pin cannot match.
func removeLoaded(ctx context.Context, cli *client.Client, image string) {
	if _, err := cli.ImageRemove(ctx, image, client.ImageRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		slog.WarnContext(ctx, "image cache: could not remove a failed load", "image", image, "error", err)
	}
}
