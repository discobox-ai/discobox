package dockerworker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/devimage"
)

// mobyExporter is the image exporter dockerd's embedded BuildKit registers
// (builder-next mobyexporter.Moby). It writes the result straight into the
// daemon's own image store, so a built image needs no tarball export and no
// ImageLoad round trip. Standalone buildkitd's "docker" exporter does not exist
// on an embedded builder, and asking for it fails with "could not be found".
const mobyExporter = "moby"

// buildImages builds development images directly on the destination Docker
// daemon through its embedded BuildKit, so no Docker daemon is needed on the
// host. This is what makes development work on Windows and macOS, where the
// copy-mode source daemon does not exist.
//
// BuildKit is required rather than the legacy builder because the development
// Dockerfiles use BuildKit-only features (a pinned `# syntax=` frontend,
// `RUN --mount=type=cache`, and $BUILDPLATFORM cross-compilation).
func buildImages(ctx context.Context, destination *client.Client, daemonID string, images []devimage.Image) error {
	ordered, err := orderBuilds(images)
	if err != nil {
		return err
	}

	// The BuildKit grpc session rides the same transport as the Docker API, so
	// VM drivers reach it over their existing tunnel. This requires a Docker
	// client whose own base transport carries the driver's dialer; see
	// NewDockerClientForDialer.
	bk, err := bkclient.New(ctx, "", bkclient.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return destination.DialHijack(ctx, "/grpc", "h2c", nil)
	}))
	if err != nil {
		return fmt.Errorf("connect to BuildKit on Docker daemon %s: %w", daemonID, err)
	}
	defer func() {
		_ = bk.Close()
	}()

	for _, image := range ordered {
		started := time.Now()
		slog.InfoContext(ctx, "building development Docker image",
			"daemon_id", daemonID,
			"image", image.Reference,
			"dockerfile", image.Build.Dockerfile)
		if err := buildImage(ctx, bk, image); err != nil {
			return fmt.Errorf("build development image %s on Docker daemon %s: %w", image.Reference, daemonID, err)
		}
		slog.InfoContext(ctx, "built development Docker image",
			"daemon_id", daemonID,
			"image", image.Reference,
			"duration", time.Since(started))
	}
	return nil
}

func buildImage(ctx context.Context, bk *bkclient.Client, image devimage.Image) error {
	spec := image.Build
	attrs := map[string]string{"filename": spec.Dockerfile}
	if platform := strings.TrimSpace(spec.Platform); platform != "" {
		attrs["platform"] = platform
	}
	if target := strings.TrimSpace(spec.Target); target != "" {
		attrs["target"] = target
	}
	for key, value := range spec.Args {
		attrs["build-arg:"+key] = value
	}

	opt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalDirs:     map[string]string{"context": spec.Context, "dockerfile": spec.Context},
		Exports: []bkclient.ExportEntry{{
			Type:  mobyExporter,
			Attrs: map[string]string{"name": image.Reference},
		}},
	}

	// Solve closes statusCh; draining it is required or the solve blocks.
	// The build's own output is captured as it streams: without it a failure
	// reports only "exit code: 1" and the compiler error that actually explains
	// it is lost.
	statusCh := make(chan *bkclient.SolveStatus)
	drained := make(chan struct{})
	var output buildLog
	go func() {
		defer close(drained)
		for status := range statusCh {
			for _, vertex := range status.Vertexes {
				if vertex.Error != "" {
					slog.WarnContext(ctx, "development image build step failed",
						"image", image.Reference, "step", vertex.Name, "error", vertex.Error)
				}
			}
			for _, entry := range status.Logs {
				output.Write(entry.Data)
			}
		}
	}()
	_, err := bk.Solve(ctx, nil, opt, statusCh)
	<-drained
	if err != nil {
		if tail := output.String(); tail != "" {
			return fmt.Errorf("%w\nbuild output:\n%s", err, tail)
		}
	}
	return err
}

// buildLogLimit bounds retained build output. Only the tail matters — that is
// where the failing command reports — and an image build can otherwise emit
// megabytes of progress.
const buildLogLimit = 16 << 10

// buildLog keeps the last buildLogLimit bytes written to it.
type buildLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *buildLog) Write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, p...)
	if len(l.data) > buildLogLimit {
		l.data = l.data[len(l.data)-buildLogLimit:]
	}
}

func (l *buildLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(string(l.data))
}

// orderBuilds sorts images so that an image whose build arguments reference
// another image in the set is built after it. Harness images are built
// FROM the sandbox base image this way, passed as a build argument.
func orderBuilds(images []devimage.Image) ([]devimage.Image, error) {
	remaining := make([]devimage.Image, len(images))
	copy(remaining, images)
	sort.Slice(remaining, func(i, j int) bool {
		return remaining[i].Reference < remaining[j].Reference
	})

	pending := make(map[string]struct{}, len(remaining))
	for _, image := range remaining {
		pending[image.Reference] = struct{}{}
	}

	ordered := make([]devimage.Image, 0, len(remaining))
	for len(remaining) > 0 {
		progressed := false
		keep := remaining[:0:0]
		for _, image := range remaining {
			if dependsOnPending(image, pending) {
				keep = append(keep, image)
				continue
			}
			ordered = append(ordered, image)
			delete(pending, image.Reference)
			progressed = true
		}
		if !progressed {
			unresolved := make([]string, 0, len(keep))
			for _, image := range keep {
				unresolved = append(unresolved, image.Reference)
			}
			return nil, fmt.Errorf("development image build dependencies form a cycle: %s", strings.Join(unresolved, ", "))
		}
		remaining = keep
	}
	return ordered, nil
}

func dependsOnPending(image devimage.Image, pending map[string]struct{}) bool {
	if image.Build == nil {
		return false
	}
	for _, value := range image.Build.Args {
		if value == image.Reference {
			continue
		}
		if _, ok := pending[value]; ok {
			return true
		}
	}
	return false
}
