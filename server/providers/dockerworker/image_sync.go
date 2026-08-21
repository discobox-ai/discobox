package dockerworker

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/discobox-ai/discobox/devimage"
	"github.com/moby/moby/client"
	"golang.org/x/sync/singleflight"
)

type dockerClientSource func() (*client.Client, error)

// DevelopmentImageSynchronizer converges watcher-built images from the
// developer's Docker daemon onto every Docker daemon used by a pool provider.
// It is intentionally a dockerworker concern: drivers provide connectivity,
// while the engine owns the Docker daemon's contents.
type DevelopmentImageSynchronizer struct {
	images      []devimage.Image
	fingerprint string
	source      dockerClientSource
	group       singleflight.Group
}

// NewDevelopmentImageSynchronizer creates an image synchronizer whose source
// daemon follows the standard DOCKER_HOST/TLS environment.
func NewDevelopmentImageSynchronizer(images []devimage.Image) (*DevelopmentImageSynchronizer, error) {
	return newDevelopmentImageSynchronizer(images, func() (*client.Client, error) {
		return client.New(client.FromEnv)
	})
}

func newDevelopmentImageSynchronizer(images []devimage.Image, source dockerClientSource) (*DevelopmentImageSynchronizer, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if source == nil {
		return nil, errors.New("development image source is required")
	}
	manifest, err := devimage.NewManifest(images)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &DevelopmentImageSynchronizer{
		images:      manifest.Images,
		fingerprint: hex.EncodeToString(sum[:]),
		source:      source,
	}, nil
}

// KeepReferences names every development image, by reference and by ID, for the
// image reaper's keep set.
//
// These have to survive on age alone, because the container check cannot see
// that they are needed: nothing ever runs the sandbox-agent base image directly,
// yet every harness image is built FROM it, so reclaiming it would break the
// next harness build. Both spellings are returned because an image may be named
// either way and the reaper matches whichever it finds.
func (s *DevelopmentImageSynchronizer) KeepReferences() []string {
	if s == nil {
		return nil
	}
	references := make([]string, 0, 2*len(s.images))
	for _, image := range s.images {
		if reference := strings.TrimSpace(image.Reference); reference != "" {
			references = append(references, reference)
		}
		if id := strings.TrimSpace(image.ID); id != "" {
			references = append(references, id)
		}
	}
	return references
}

// Ensure converges the configured development images onto destination. Calls
// for the same Docker daemon and manifest coalesce so concurrent pool
// reconciles do not upload the same archive more than once.
func (s *DevelopmentImageSynchronizer) Ensure(ctx context.Context, destination *client.Client) error {
	if s == nil {
		return nil
	}
	if destination == nil {
		return errors.New("development image destination is required")
	}
	info, err := destination.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("inspect destination Docker daemon: %w", err)
	}
	daemonID := strings.TrimSpace(info.Info.ID)
	if daemonID == "" {
		return errors.New("destination Docker daemon reported no ID")
	}
	_, err, _ = s.group.Do(daemonID+":"+s.fingerprint, func() (any, error) {
		return nil, s.ensure(ctx, daemonID, destination)
	})
	return err
}

func (s *DevelopmentImageSynchronizer) ensure(ctx context.Context, daemonID string, destination *client.Client) error {
	missing := make([]devimage.Image, 0, len(s.images))
	missingBuilds := make([]devimage.Image, 0, len(s.images))
	for _, image := range s.images {
		if image.Build != nil {
			// Build-mode references are unique per development build, so the
			// reference existing on the destination is itself proof of
			// freshness; there is no source daemon to compare an ID against.
			_, err := destination.ImageInspect(ctx, image.Reference)
			switch {
			case err == nil:
				continue
			case !cerrdefs.IsNotFound(err):
				return fmt.Errorf("inspect development image %s on Docker daemon %s: %w", image.Reference, daemonID, err)
			}
			missingBuilds = append(missingBuilds, image)
			continue
		}

		inspect, err := destination.ImageInspect(ctx, image.Reference)
		switch {
		case err == nil && inspect.ID == image.ID:
			continue
		case err != nil && !cerrdefs.IsNotFound(err):
			return fmt.Errorf("inspect development image %s on Docker daemon %s: %w", image.Reference, daemonID, err)
		}

		inspectByID, err := destination.ImageInspect(ctx, image.ID)
		switch {
		case err == nil && inspectByID.ID == image.ID:
			if _, err := destination.ImageTag(ctx, client.ImageTagOptions{Source: image.ID, Target: image.Reference}); err != nil {
				return fmt.Errorf("tag development image %s on Docker daemon %s: %w", image.Reference, daemonID, err)
			}
		case err != nil && !cerrdefs.IsNotFound(err):
			return fmt.Errorf("inspect development image ID %s on Docker daemon %s: %w", image.ID, daemonID, err)
		default:
			missing = append(missing, image)
		}
	}

	if len(missingBuilds) > 0 {
		if err := buildImages(ctx, destination, daemonID, missingBuilds); err != nil {
			return err
		}
	}
	if len(missing) == 0 {
		return nil
	}

	// Only copy-mode images reach here, so the source daemon is opened only when
	// one is actually needed. A build-mode-only manifest never touches it, which
	// is what lets a host without Docker (Windows, macOS) converge images.
	source, err := s.source()
	if err != nil {
		return fmt.Errorf("open development image source Docker daemon: %w", err)
	}
	defer func() {
		_ = source.Close()
	}()

	references := make([]string, 0, len(missing))
	for _, image := range missing {
		inspect, err := source.ImageInspect(ctx, image.Reference)
		if err != nil {
			return fmt.Errorf("inspect source development image %s: %w", image.Reference, err)
		}
		if inspect.ID != image.ID {
			return fmt.Errorf("source development image %s has ID %s, manifest requires %s", image.Reference, inspect.ID, image.ID)
		}
		references = append(references, image.Reference)
	}

	started := time.Now()
	slog.InfoContext(ctx, "synchronizing development Docker images",
		"daemon_id", daemonID,
		"images", references)
	if err := transferImages(ctx, source, destination, references); err != nil {
		return fmt.Errorf("synchronize development images to Docker daemon %s: %w", daemonID, err)
	}
	for _, image := range missing {
		inspect, err := destination.ImageInspect(ctx, image.Reference)
		if err != nil {
			return fmt.Errorf("verify development image %s on Docker daemon %s: %w", image.Reference, daemonID, err)
		}
		if inspect.ID != image.ID {
			return fmt.Errorf("development image %s on Docker daemon %s has ID %s after load, want %s", image.Reference, daemonID, inspect.ID, image.ID)
		}
	}
	slog.InfoContext(ctx, "synchronized development Docker images",
		"daemon_id", daemonID,
		"images", references,
		"duration", time.Since(started))
	return nil
}

func transferImages(ctx context.Context, source, destination *client.Client, references []string) error {
	archive, err := source.ImageSave(ctx, references)
	if err != nil {
		return fmt.Errorf("save source images: %w", err)
	}
	defer archive.Close()

	reader, writer := io.Pipe()
	type compressionResult struct {
		bytes int64
		err   error
	}
	compressed := make(chan compressionResult, 1)
	go func() {
		gzipWriter, gzipErr := gzip.NewWriterLevel(writer, gzip.BestSpeed)
		if gzipErr != nil {
			_ = writer.CloseWithError(gzipErr)
			compressed <- compressionResult{err: gzipErr}
			return
		}
		written, copyErr := io.Copy(gzipWriter, archive)
		closeErr := gzipWriter.Close()
		compressionErr := errors.Join(copyErr, closeErr)
		_ = writer.CloseWithError(compressionErr)
		compressed <- compressionResult{bytes: written, err: compressionErr}
	}()

	load, loadErr := destination.ImageLoad(ctx, reader, client.ImageLoadWithQuiet(true))
	if loadErr != nil {
		_ = reader.CloseWithError(loadErr)
		result := <-compressed
		return errors.Join(loadErr, result.err)
	}
	_, responseErr := io.Copy(io.Discard, load)
	closeErr := load.Close()
	_ = reader.Close()
	result := <-compressed
	if err := errors.Join(result.err, responseErr, closeErr); err != nil {
		return err
	}
	slog.DebugContext(ctx, "transferred development Docker image archive", "uncompressed_bytes", result.bytes)
	return nil
}
