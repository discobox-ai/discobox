package discovm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/vm/pkg/build"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// guestImage is one disco-vm image a driver's pools boot: the build spec in a
// discobox checkout, and the tag the engine finds the result by.
type guestImage struct {
	Tag string
	// Spec is the build spec's path in the checkout, slash-separated. The
	// checkout is the build's context, so a spec copies in what it names
	// relative to the checkout's root.
	Spec string
}

// BuildGuestImage builds the driver's images into this provider's engine, from
// the specs in a discobox checkout, one after another.
//
// The pool is only where the operation was asked from. Unlike a dockerworker
// guest image, which a running pool builds on its own Docker daemon, a disco-vm
// image is built by the engine in the server, and every pool of the driver
// boots it.
func (r *Runtime) BuildGuestImage(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	images := r.driver.images()
	if len(images) == 0 {
		return nil, fmt.Errorf("the %s driver defines no disco-vm image to build yet: %w", r.engine.Driver.Name(), sandbox.ErrGuestImageBuildUnsupported)
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
	specs := make([]string, len(images))
	for i, image := range images {
		specs[i] = filepath.Join(source, filepath.FromSlash(image.Spec))
		if info, err := os.Stat(specs[i]); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s holds no %s, so it is not a discobox checkout with this driver's images", source, image.Spec)
		}
	}

	reader, writer := io.Pipe()
	go func() {
		builder := &build.Builder{Engine: r.engine, Out: writer}
		for i, image := range images {
			fmt.Fprintf(writer, "building %s from %s\n", image.Tag, image.Spec)
			if _, err := builder.Build(ctx, build.Options{File: specs[i], Context: source, Tags: []string{image.Tag}}); err != nil {
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
