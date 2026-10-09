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

	"github.com/discobox-ai/vm/pkg/build"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// imagesDir is where a discobox checkout keeps the disco-vm build specs for
// each driver, in a directory named as disco-vm names the driver. A spec is
// <role>.yaml, and builds the image imageTag names: pool.yaml is the pool
// machine's (poolRole).
const imagesDir = "server/providers/discovm/images"

// imageTag is the tag a driver's image of a role has in the engine's store.
// It names the driver because every driver on a host shares one store, and one
// tag namespace: a tag per role alone would move from one driver's image to
// another's whenever both were built, and the first driver's next create would
// be refused for an image built for the other.
func imageTag(driver, role string) string {
	return "discobox-" + driver + "-" + role
}

// guestImage is one disco-vm image to build: the build spec in a discobox
// checkout, and the tag the engine finds the result by.
type guestImage struct {
	Tag string
	// Spec is the build spec's path in the checkout, slash-separated. The
	// checkout is the build's context, so a spec copies in what it names
	// relative to the checkout's root.
	Spec string
}

// driverImages lists the build specs a checkout holds for a driver. Which
// images there are is the checkout's to say, not this package's.
func driverImages(source, driver string) ([]guestImage, error) {
	dir := path.Join(imagesDir, driver)
	entries, err := os.ReadDir(filepath.Join(source, filepath.FromSlash(dir)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var images []guestImage
	for _, entry := range entries {
		role, ok := strings.CutSuffix(entry.Name(), ".yaml")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		images = append(images, guestImage{Tag: imageTag(driver, role), Spec: path.Join(dir, entry.Name())})
	}
	return images, nil
}

// BuildGuestImage builds the driver's images into this provider's engine, from
// the specs a discobox checkout holds for it, one after another.
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
	images, err := driverImages(source, driver)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("%s holds no disco-vm image spec for the %s driver under %s/%s: %w", source, driver, imagesDir, driver, sandbox.ErrGuestImageBuildUnsupported)
	}
	specs := make([]string, len(images))
	for i, image := range images {
		specs[i] = filepath.Join(source, filepath.FromSlash(image.Spec))
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
