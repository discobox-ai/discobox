package harnessconfigs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	dockerclient "github.com/moby/moby/client"
)

type imageMetadata struct {
	Digest string
	harness.ImageMetadata
}

type imageInspector interface {
	Inspect(context.Context, string) (imageMetadata, error)
}

type defaultImageInspector struct{}

func (defaultImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	// Prefer a locally present image. Once the daemon has inspected it, its
	// metadata is authoritative — surface any label error instead of masking it
	// with a doomed registry pull of the same (often :local) reference.
	if local, found, err := inspectLocalImage(ctx, imageRef); found {
		return local, err
	}
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("parse harness image %q: %w", imageRef, err)
	}
	image, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return imageMetadata{}, fmt.Errorf("inspect harness image %q: %w", imageRef, err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read harness image config %q: %w", imageRef, err)
	}
	// Use the config digest so the recorded digest matches the local daemon's
	// image ID for the same image; the manifest digest is only defined once an
	// image is pushed, which local :local builds never are.
	digest, err := image.ConfigName()
	if err != nil {
		return imageMetadata{}, fmt.Errorf("resolve harness image digest %q: %w", imageRef, err)
	}
	return parseImageMetadata(digest.String(), config.Config.Labels)
}

// inspectLocalImage inspects imageRef via the local Docker daemon. found is true
// only when the daemon returned an image (regardless of whether its label parses),
// so the caller can surface label errors for present images and fall back to the
// registry only when the image is genuinely unavailable locally.
func inspectLocalImage(ctx context.Context, imageRef string) (imageMetadata, bool, error) {
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return imageMetadata{}, false, err
	}
	defer client.Close()
	inspected, err := client.ImageInspect(ctx, imageRef)
	if err != nil {
		return imageMetadata{}, false, err
	}
	labels := map[string]string(nil)
	if inspected.Config != nil {
		labels = inspected.Config.Labels
	}
	metadata, err := parseImageMetadata(inspected.ID, labels)
	return metadata, true, err
}

func parseImageMetadata(digest string, labels map[string]string) (imageMetadata, error) {
	raw := strings.TrimSpace(labels[harness.ImageLabel])
	if raw == "" {
		return imageMetadata{}, fmt.Errorf("image is missing required label %q", harness.ImageLabel)
	}
	var metadata harness.ImageMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return imageMetadata{}, fmt.Errorf("parse %s label: %w", harness.ImageLabel, err)
	}
	if err := validateImageMetadata(metadata); err != nil {
		return imageMetadata{}, err
	}
	return imageMetadata{Digest: digest, ImageMetadata: metadata}, nil
}

func validateImageMetadata(metadata harness.ImageMetadata) error {
	if metadata.Harness == nil {
		return fmt.Errorf("%s label requires harness", harness.ImageLabel)
	}
	h := metadata.Harness
	if strings.TrimSpace(h.ID) == "" {
		return fmt.Errorf("%s label requires harness id", harness.ImageLabel)
	}
	if strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("%s label requires harness name", harness.ImageLabel)
	}
	// An omitted runCommand is a declaration, not an omission: it means the
	// sandbox resolves the user's login shell, which is the only place that
	// knows whether it is bash, zsh, or fish (ADR 0025 §3, ADR 0043). A
	// *present but blank* command is still a broken image.
	if len(h.RunCommand) > 0 && strings.TrimSpace(h.RunCommand[0]) == "" {
		return fmt.Errorf("%s label has a blank runCommand", harness.ImageLabel)
	}
	if h.Config != nil && (len(h.Config.Command) == 0 || strings.TrimSpace(h.Config.Command[0]) == "") {
		return fmt.Errorf("%s label config mode requires command", harness.ImageLabel)
	}
	seen := map[string]struct{}{}
	for _, secret := range h.Secrets {
		name := strings.TrimSpace(secret.Name)
		if !services.HarnessConfigEnvVarNamePattern.MatchString(name) {
			return fmt.Errorf("%s label has invalid secret environment variable %q", harness.ImageLabel, secret.Name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s label has duplicate secret %q", harness.ImageLabel, name)
		}
		seen[name] = struct{}{}
	}
	for idx, volume := range metadata.Volumes {
		if strings.TrimSpace(volume.Path) == "" {
			return fmt.Errorf("%s label volume[%d] requires path", harness.ImageLabel, idx)
		}
		switch volume.Volume {
		case harness.VolumeData, harness.VolumeCache:
		default:
			return fmt.Errorf("%s label volume %q has unknown kind %q", harness.ImageLabel, volume.Path, volume.Volume)
		}
	}
	return nil
}

// harnessMetadataBuildArg is the build argument each harness Dockerfile turns
// into the harness.ImageLabel label. Keep the two in sync.
const harnessMetadataBuildArg = "HARNESS_METADATA"

// devImageInspector resolves harness metadata from the development image
// manifest before falling back to a daemon or registry lookup.
//
// In build-mode the host has no Docker daemon and the dev image has never been
// pushed, so neither fallback can see it: the image exists only as a build
// description until some pool's daemon builds it. The manifest already carries
// the metadata verbatim, as the build argument that becomes the label, so
// seeding reads it from there and no longer depends on the image existing yet.
type devImageInspector struct {
	metadataByReference map[string]string
	fallback            imageInspector
}

// newDevImageInspector wraps fallback with the build-mode manifest entries in
// images. It returns fallback unchanged when no entry carries harness metadata,
// so copy-mode and production keep the original behavior exactly.
func newDevImageInspector(images []devimage.Image, fallback imageInspector) imageInspector {
	metadata := map[string]string{}
	for _, image := range images {
		if image.Build == nil {
			continue
		}
		if raw := strings.TrimSpace(image.Build.Args[harnessMetadataBuildArg]); raw != "" {
			metadata[strings.TrimSpace(image.Reference)] = raw
		}
	}
	if len(metadata) == 0 {
		return fallback
	}
	return devImageInspector{metadataByReference: metadata, fallback: fallback}
}

func (d devImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	raw, ok := d.metadataByReference[strings.TrimSpace(imageRef)]
	if !ok {
		return d.fallback.Inspect(ctx, imageRef)
	}
	// A build-mode reference is content-addressed over that image's inputs, so
	// it is its own freshness key; there is no digest until it is built.
	return parseImageMetadata(imageRef, map[string]string{harness.ImageLabel: raw})
}
