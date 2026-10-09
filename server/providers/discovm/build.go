package discovm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/build"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// twin is one disco-vm image in a discobox checkout: the build spec beside the
// Dockerfile it is the twin of (<dir>/<driver>.yaml), the directory it is
// built from, and the tag the engine finds it by. A twin built FROM another
// names that one's tag in its own `from:`, so the tags are the specs' contract,
// not this package's choice.
type twin struct {
	Tag string
	// Dir holds the Dockerfile and its twins; Context is the build's context.
	// Both are slash-separated paths in the checkout.
	Dir     string
	Context string
}

// Spec is the twin's build spec for a driver.
func (t twin) Spec(driver string) string { return path.Join(t.Dir, driver+".yaml") }

// twins is the chain BuildGuestImage builds, parents before children, as
// `build:boxd-images` in Taskfile.yml builds it for boxd (which
// TestTwinsMatchTheTaskfile holds it to). A driver builds the twins it has a
// spec for, and skips the rest: the pool agent's has none yet (#123).
var twins = []twin{
	{Tag: "discobox/base", Dir: "base-image", Context: "base-image"},
	{Tag: poolImage, Dir: "pool-agent", Context: "."},
	{Tag: "discobox/sandbox-agent", Dir: "sandbox-agent", Context: "."},
	{Tag: "discobox/claude-code", Dir: "harness/claude-code", Context: "harness/claude-code"},
	{Tag: "discobox/codex", Dir: "harness/codex-cli", Context: "harness/codex-cli"},
	{Tag: "discobox/opencode", Dir: "harness/opencode", Context: "harness/opencode"},
	{Tag: "discobox/copilot", Dir: "harness/copilot", Context: "harness/copilot"},
}

// driverTwins lists the twins a checkout holds a spec for, for a driver.
func driverTwins(source, driver string) ([]twin, error) {
	var out []twin
	for _, t := range twins {
		info, err := os.Stat(filepath.Join(source, filepath.FromSlash(t.Spec(driver))))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			out = append(out, t)
		}
	}
	return out, nil
}

// BuildGuestImage builds the driver's twins into this provider's engine, from
// a discobox checkout, parents first.
//
// The pool is only where the operation was asked from. Unlike a dockerworker
// guest image, which a running pool builds on its own Docker daemon, a disco-vm
// image is built by the engine in the server, and every pool of the driver
// boots it.
func (r *Runtime) BuildGuestImage(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	if opts.RestartHost {
		// A machine is cloned from the image it was created from, so a new
		// image reaches a pool only by replacing its machine, which a build
		// does not do.
		return nil, fmt.Errorf("a %s pool boots a new image only once its machine is replaced, which a build does not do; build without restarting: %w", ProviderType, sandbox.ErrGuestImageRestartUnsupported)
	}
	source, err := checkoutDir(opts.SourceDir)
	if err != nil {
		return nil, err
	}
	driver := r.engine.Driver.Name()
	images, err := driverTwins(source, driver)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("%s holds no disco-vm build spec for the %s driver (a %s.yaml beside a Dockerfile): %w", source, driver, driver, sandbox.ErrGuestImageBuildUnsupported)
	}

	reader, writer := io.Pipe()
	go func() {
		release, err := lockChain(ctx, r.engine.Root, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		defer release()
		builder := &build.Builder{Engine: r.engine, Out: writer}
		for _, image := range images {
			fmt.Fprintf(writer, "building %s from %s\n", image.Tag, image.Spec(driver))
			opts := build.Options{
				File:    filepath.Join(source, filepath.FromSlash(image.Spec(driver))),
				Context: filepath.Join(source, filepath.FromSlash(image.Context)),
				Tags:    []string{image.Tag},
			}
			if _, err := builder.Build(ctx, opts); err != nil {
				// The reader's next Read returns the build's error rather
				// than io.EOF, so nothing has to read the text to know.
				_ = writer.CloseWithError(fmt.Errorf("build %s: %w", image.Tag, err))
				return
			}
		}
		_ = writer.Close()
	}()
	return &sandbox.GuestImageBuild{Destination: r.engine.Images.Root, ReadCloser: reader}, nil
}

// chainLockPoll is how often a chain waiting for the root's build lock asks
// again. A chain takes minutes, so a second is quick enough.
const chainLockPoll = time.Second

// lockChain serializes build chains per engine root, across every process that
// shares it, by a file lock in the root. A chain resolves each parent by a tag
// the chain itself moves, and disco-vm locks only each tag update: two chains
// at once, from two provider instances or two servers whose data directories
// differ but whose user's disco-vm root does not, would build one's children on
// the other's parents. A file lock is per open file, so it also holds two
// chains in this process apart. A waiting chain says so on out once, and gives
// up when ctx ends.
func lockChain(ctx context.Context, root string, out io.Writer) (func(), error) {
	path := filepath.Join(root, "build.lock")
	said := false
	for {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			return func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		if !said {
			fmt.Fprintf(out, "waiting for another image build in %s to finish\n", root)
			said = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(chainLockPoll):
		}
	}
}

// checkoutDir checks the directory a build was asked to read: an absolute path
// to a directory on the machine running the control plane.
func checkoutDir(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("a source directory is required: the image is built from a checkout on the machine running the control plane")
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("image source directory %q must be an absolute path", source)
	}
	source = filepath.Clean(source)
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return "", fmt.Errorf("image source directory %s is not a directory on the machine running the control plane", source)
	}
	return source, nil
}
