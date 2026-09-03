package dockerworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// localExporter writes a build's final stage to the client's filesystem instead
// of into an image store. It is what turns a `FROM scratch` stage carrying a
// kernel, an initrd, and a root filesystem into three files on the host: the
// files travel back over the same grpc session the build ran on, so a Mac with
// no Docker daemon receives them from a daemon inside a VM (ADR 0062 §7).
const localExporter = "local"

// GuestImageBuildSpec describes one guest image build for a driver that has one.
//
// The driver owns every field: which Dockerfile in the checkout builds its
// guest, what platform that guest runs on, and where a local build has to land
// for the driver's own resolver to prefer it. The engine owns only the parts
// that are the same for any build on a pool — reaching BuildKit through the
// driver's transport, and rendering the build for whoever asked for it.
type GuestImageBuildSpec struct {
	// Dockerfile is the guest image Dockerfile, relative to the source
	// directory, in slash form.
	Dockerfile string
	// Platform is the guest's platform, which is the pool host's rather than
	// the caller's: the artifacts boot on that machine's hypervisor.
	Platform string
	// Destination is where the finished artifacts are published, replacing
	// whatever is there. It is the directory the driver's guest resolver
	// prefers over the published image.
	Destination string
	// Adopt is called once the artifacts are in place, for a driver that caches
	// what it resolved. Optional.
	Adopt func()
}

// BuildGuestImage builds a driver's guest image on the pool's own Docker daemon
// and exports the artifacts to the host.
//
// The stream is the build as it happens. Nothing waits for the build before
// returning: the caller is a developer watching minutes of work on a machine
// they cannot see, and a call that returned only at the end would be the silent
// wait this exists to remove.
func (e *Engine) BuildGuestImage(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	if pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, errors.New("pool is required")
	}
	spec, err := e.driver.GuestImageBuildSpec()
	if err != nil {
		return nil, err
	}
	source, err := guestImageSourceDir(opts.SourceDir, spec.Dockerfile)
	if err != nil {
		return nil, err
	}

	// The lease is held for the life of the build, not of this call, so it is
	// released by the stream rather than deferred here.
	lease, err := e.acquireDockerReady(ctx, pool.ID)
	if err != nil {
		return nil, fmt.Errorf("reach the pool's Docker daemon: %w", err)
	}
	bk, err := connectBuildKit(ctx, lease.Client, pool.ID)
	if err != nil {
		lease.Release()
		return nil, err
	}

	reader, writer := io.Pipe()
	stream := &guestImageBuildStream{reader: reader, writer: writer, release: func() {
		_ = bk.Close()
		lease.Release()
	}}
	go func() {
		err := solveGuestImage(ctx, bk, source, spec, writer)
		if err == nil && opts.RestartHost {
			err = e.restartPoolHost(ctx, pool.ID, writer)
		}
		stream.finish(err)
	}()
	return &sandbox.GuestImageBuild{Destination: spec.Destination, ReadCloser: stream}, nil
}

// solveGuestImage runs the build, exports it beside its destination, and swaps
// it into place.
//
// Staging and renaming is the whole of the safety here: a pool may be booting
// from the destination directory at this moment, and a build that wrote into it
// directly would replace a kernel underneath a VM that is reading it.
func solveGuestImage(ctx context.Context, bk *bkclient.Client, source string, spec GuestImageBuildSpec, out *io.PipeWriter) error {
	parent := filepath.Dir(spec.Destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create guest image directory: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".build-")
	if err != nil {
		return fmt.Errorf("create guest image staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	attrs := map[string]string{"filename": spec.Dockerfile}
	if platform := strings.TrimSpace(spec.Platform); platform != "" {
		attrs["platform"] = platform
	}
	solveOpt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: attrs,
		LocalDirs:     map[string]string{"context": source, "dockerfile": source},
		Exports:       []bkclient.ExportEntry{{Type: localExporter, OutputDir: staging}},
	}

	// progressui renders the same plain output `docker build` prints, which is
	// what someone watching a build expects to read and what makes a failing
	// step legible without this package knowing anything about the Dockerfile.
	display, err := progressui.NewDisplay(out, progressui.PlainMode)
	if err != nil {
		return fmt.Errorf("render guest image build: %w", err)
	}
	status := make(chan *bkclient.SolveStatus)
	rendered := make(chan struct{})
	go func() {
		defer close(rendered)
		// Errors here are the renderer's, not the build's; the build reports
		// its own below.
		_, _ = display.UpdateFrom(ctx, status)
	}()
	_, solveErr := bk.Solve(ctx, nil, solveOpt, status)
	<-rendered
	if solveErr != nil {
		return solveErr
	}

	if err := publishGuestImage(staging, spec.Destination); err != nil {
		return err
	}
	if spec.Adopt != nil {
		spec.Adopt()
	}
	_, _ = fmt.Fprintf(out, "guest image artifacts written to %s\n", spec.Destination)
	return nil
}

// restartPoolHost stops the pool's host so the pool's reconcile brings it back
// on the guest image that was just built.
//
// Stopping is the whole of it, and StopVM rather than DeleteVM is why it is
// safe: the disks survive, so the pool keeps its images, volumes and
// containers, and the reconciler already watching this pool starts the machine
// again. A running VM boots the artifacts it was started with, so without this
// a successful build changes nothing an operator can see — which reads as a
// build that did not work.
func (e *Engine) restartPoolHost(ctx context.Context, poolID string, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "stopping the pool host so it boots the guest image just built\n")
	if err := e.driver.StopVM(ctx, poolID); err != nil {
		return fmt.Errorf("stop the pool host to adopt the built guest image: %w", err)
	}
	_, _ = fmt.Fprintf(out, "pool host stopped; the pool's reconcile starts it again on the new guest\n")
	return nil
}

// publishGuestImage swaps the staged artifacts in, keeping the previous
// directory only long enough for the move to succeed.
func publishGuestImage(staging, destination string) error {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("read built guest image artifacts: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("the guest image build exported no artifacts: its final stage is empty")
	}
	previous := destination + ".previous"
	_ = os.RemoveAll(previous)
	if err := os.Rename(destination, previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move the previous guest image aside: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		// Put back whatever was there rather than leaving the driver with no
		// local guest at all.
		_ = os.Rename(previous, destination)
		return fmt.Errorf("publish the built guest image: %w", err)
	}
	_ = os.RemoveAll(previous)
	return nil
}

// guestImageSourceDir validates the checkout a build was pointed at, so a
// mistyped path fails here rather than as a BuildKit error about a context that
// could not be read.
func guestImageSourceDir(source, dockerfile string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", errors.New("a source directory is required: the guest image is built from a checkout on the machine running the control plane")
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("guest image source directory %q must be an absolute path", source)
	}
	source = filepath.Clean(source)
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return "", fmt.Errorf("guest image source directory %s is not a directory on the machine running the control plane", source)
	}
	path := filepath.Join(source, filepath.FromSlash(dockerfile))
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s holds no %s, so it is not a discobox checkout", source, dockerfile)
	}
	return source, nil
}

// guestImageBuildStream is the build's output, and the lifetime of everything
// the build holds. Closing it early ends the read; the build itself is ended by
// the context its caller gave.
type guestImageBuildStream struct {
	reader  *io.PipeReader
	writer  *io.PipeWriter
	release func()
}

func (s *guestImageBuildStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *guestImageBuildStream) Close() error {
	// The writer is closed first so a build still rendering into the pipe fails
	// its next write rather than blocking on a reader that has gone.
	_ = s.writer.Close()
	err := s.reader.Close()
	s.release()
	return err
}

// finish ends the output, carrying a failed build's error to the reader as the
// stream's own error. A route turns that into its out-of-band outcome; nothing
// has to read the build's text to know it failed.
func (s *guestImageBuildStream) finish(err error) {
	// Closing the write half is what the reader sees: with an error, its next
	// Read returns that error instead of io.EOF.
	_ = s.writer.CloseWithError(err)
}
