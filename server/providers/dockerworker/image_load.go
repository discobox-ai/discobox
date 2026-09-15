package dockerworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"

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
func (e *Engine) loadFromCache(ctx context.Context, lease *DockerClientLease, poolID, image string, phase sandbox.PoolProvisionPhase) bool {
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
	if err := e.importCachedImage(ctx, lease.Client, poolID, image, cached, usesContainerdStore(info.Info), phase); err != nil {
		slog.WarnContext(ctx, "image cache: load failed; pulling instead", "pool", poolID, "image", image, "error", err)
		return false
	}
	return true
}

// importCachedImage imports and verifies the cached identity on either Docker
// image store. Failure removes the partial image so a later retry can recover.
func (e *Engine) importCachedImage(ctx context.Context, cli *client.Client, poolID, image string, cached *imagecache.Image, containerd bool, phase sandbox.PoolProvisionPhase) (err error) {
	started := time.Now()
	defer func() {
		if err != nil {
			removeLoaded(ctx, cli, image)
		}
	}()
	if err := e.loadCached(ctx, cli, poolID, cached, phase); err != nil {
		return err
	}
	if !containerd {
		if err := recordRegistryDigest(ctx, cli, cached); err != nil {
			return fmt.Errorf("record registry digest: %w", err)
		}
	}
	if err := verifyLoaded(ctx, cli, image, cached.Digest()); err != nil {
		return err
	}
	slog.InfoContext(ctx, "loaded image from the image cache", "pool", poolID, "image", image,
		"digest", cached.Digest(), "bytes", cached.Size(), "duration", time.Since(started))
	return nil
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
// written as a pull reports the bytes downloaded, and then the layers the
// daemon extracts from it.
func (e *Engine) loadCached(ctx context.Context, cli *client.Client, poolID string, cached *imagecache.Image, phase sandbox.PoolProvisionPhase) error {
	diffIDs, err := cached.DiffIDs()
	if err != nil {
		return err
	}
	extraction := newLayerExtraction(cached.Layers, diffIDs)
	var progressMu sync.Mutex
	progress := sandbox.PoolPullProgress{Image: cached.Reference.Name(), Total: cached.Size(), Layers: len(cached.Layers)}
	report := func() {
		progressMu.Lock()
		snapshot := progress
		progressMu.Unlock()
		e.cfg.ProgressReporter.ReportProgress(ctx, poolID, sandbox.PoolProvisionProgress{Phase: phase, Pull: &snapshot})
	}
	// Once before the first byte, so the phase is on the row from the start.
	report()

	// Docker may spend minutes on one layer, and says nothing in between.
	// Keep the latest counts fresh until its response finishes.
	stopHeartbeat, heartbeatDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				report()
			}
		}
	}()
	defer func() { close(stopHeartbeat); <-heartbeatDone }()

	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() {
		// Archive progress and the extraction heartbeat share the snapshot.
		var lastReport time.Time
		err := cached.WriteArchive(writer, func(current int64, layersComplete int) {
			progressMu.Lock()
			progress.Current, progress.LayersComplete = current, layersComplete
			progressMu.Unlock()
			if time.Since(lastReport) >= poolPullReportInterval {
				lastReport = time.Now()
				report()
			}
		})
		_ = writer.CloseWithError(err)
		written <- err
	}()
	// Not quiet: the classic store reports each layer it extracts, and that is
	// most of a load, since it reads the whole archive before extracting any.
	load, err := cli.ImageLoad(ctx, reader, client.ImageLoadWithQuiet(false))
	if err != nil {
		_ = reader.CloseWithError(err)
		return errors.Join(err, <-written)
	}
	var lastExtractionReport time.Time
	loadErr := readLoadResponse(load, func(message jsonstream.Message) {
		progressMu.Lock()
		moved := extraction.apply(message)
		progress.Extracted, progress.LayersExtracted = extraction.bytes, extraction.layers
		progressMu.Unlock()
		if moved && time.Since(lastExtractionReport) >= poolPullReportInterval {
			lastExtractionReport = time.Now()
			report()
		}
	})
	_ = load.Close()
	// Closed before waiting, so an archive the daemon stopped reading ends
	// rather than blocking on a pipe nothing drains.
	_ = reader.Close()
	if err := errors.Join(<-written, loadErr); err != nil {
		return err
	}
	progressMu.Lock()
	// Whatever the daemon said along the way, every layer is in its store now.
	extraction.finish()
	progress.Extracted, progress.LayersExtracted = extraction.bytes, extraction.layers
	progress.Done = true
	progressMu.Unlock()
	report()
	return nil
}

// readLoadResponse drains a load's response, handing each message to progress.
// A load reports failure inside the stream rather than in its status code.
func readLoadResponse(body io.Reader, progress func(jsonstream.Message)) error {
	decoder := json.NewDecoder(body)
	for {
		var message struct {
			jsonstream.Message
			// Older daemons state an error only here.
			ErrorText string `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read load response: %w", err)
		}
		if message.Error != nil && message.Error.Message != "" {
			return errors.New(message.Error.Message)
		}
		if message.ErrorText != "" {
			return errors.New(message.ErrorText)
		}
		progress(message.Message)
	}
}

// loadingLayerStatus is the status the classic image store gives each layer
// it extracts during a load, with the bytes of that layer's blob it has read.
// The containerd store reports nothing until the load is over.
const loadingLayerStatus = "Loading layer"

// layerExtraction follows a classic daemon through the layers of a load.
//
// The daemon extracts the image's layers in order and skips, silently, each
// one its store already holds, so a message about a layer says that every
// layer before it is in the store too. Counting from the image's own order is
// what lets those skipped layers count: an image built on a base the pool
// already has would otherwise never reach its own layer count.
type layerExtraction struct {
	// ids are the layers' diff IDs as a message abbreviates them, and offsets
	// the bytes of every layer's blob before each one, plus the whole.
	ids     []string
	offsets []int64
	// layers is how many are in the store — every one before the layer being
	// extracted — and bytes those and what has been read of that one.
	layers int
	bytes  int64
}

func newLayerExtraction(layers []imagecache.Descriptor, diffIDs []string) *layerExtraction {
	extraction := &layerExtraction{offsets: make([]int64, len(layers)+1)}
	for i, layer := range layers {
		extraction.offsets[i+1] = extraction.offsets[i] + layer.Size
	}
	// A config that does not name a diff ID per layer gives no way to place a
	// message, so only the end of the load is counted.
	if len(diffIDs) == len(layers) {
		for _, diffID := range diffIDs {
			extraction.ids = append(extraction.ids, stringid.TruncateID(diffID))
		}
	}
	return extraction
}

// apply folds in one message and reports whether the counts moved.
func (x *layerExtraction) apply(message jsonstream.Message) bool {
	if message.Status != loadingLayerStatus || message.Progress == nil || x.ids == nil {
		return false
	}
	// The daemon restates a layer as it finishes it, which is not the start of
	// a later layer with the same ID.
	if last := x.layers - 1; last >= 0 && x.ids[last] == message.ID && message.Progress.Current >= x.size(last) {
		return false
	}
	i := slices.Index(x.ids[x.layers:], message.ID)
	if i < 0 {
		return false
	}
	at := x.layers + i
	layers, current := at, min(message.Progress.Current, x.size(at))
	if current == x.size(at) {
		layers, current = at+1, 0
	}
	bytes := x.offsets[layers] + current
	moved := layers != x.layers || bytes != x.bytes
	x.layers, x.bytes = layers, bytes
	return moved
}

func (x *layerExtraction) size(layer int) int64 {
	return x.offsets[layer+1] - x.offsets[layer]
}

// finish counts every layer as in the store, which a load that succeeded
// means whether or not the daemon said so.
func (x *layerExtraction) finish() {
	x.layers = len(x.offsets) - 1
	x.bytes = x.offsets[x.layers]
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
