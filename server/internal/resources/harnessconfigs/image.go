package harnessconfigs

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	remoteOptions := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(poolPlatform()),
	}
	// One GET, because it answers both questions: the descriptor carries the
	// digest the registry serves this tag under, and resolves to the image for
	// the platform asked for.
	//
	// Not remote.Head, which asks for exactly the digest and nothing else:
	// ghcr.io answers HEAD without a Content-Length header, which
	// go-containerregistry rejects — so every harness image "was unavailable",
	// none were seeded, and a project came up with no harnesses at all.
	descriptor, err := remote.Get(ref, remoteOptions...)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("inspect harness image %q: %w", imageRef, err)
	}
	image, err := descriptor.Image()
	if err != nil {
		return imageMetadata{}, fmt.Errorf("resolve harness image %q: %w", imageRef, err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read harness image config %q: %w", imageRef, err)
	}
	// The digest a daemon will report for this reference, which is the digest
	// the registry serves the tag under — an index digest for a multi-platform
	// image.
	//
	// It used to be the config digest, on the premise that a local Docker
	// daemon reports that as an image ID. That was true of the classic image
	// store and is false of the containerd one, which reports the index digest
	// and is the default in current Docker. So the server recorded a value the
	// daemon would never produce, and every sandbox on a published multi-arch
	// image refused to launch: "pinned to sha256:6a5066…, now resolves to
	// sha256:4a5726…" — the config digest and the index digest of one image
	// that had not changed at all.
	//
	// Both store types put this value in RepoDigests, which is what the pool
	// compares against, so one recorded digest now works on either.
	return parseImageMetadata(descriptor.Digest.String(), config.Config.Labels)
}

// localImageDigest is the digest to pin a locally-inspected image to.
//
// A pulled image carries the registry digest in RepoDigests, and that is what
// every daemon can be asked about later, whichever image store it uses. A
// locally built one has none — nothing pushed it — so its image ID is all there
// is, and it is also all the pool will have to compare against.
func localImageDigest(inspected dockerclient.ImageInspectResult) string {
	for _, repoDigest := range inspected.RepoDigests {
		if _, digest, ok := strings.Cut(repoDigest, "@"); ok && digest != "" {
			return digest
		}
	}
	return inspected.ID
}

// poolPlatform is the platform a harness image is inspected for.
//
// A multi-arch image has no single config digest — it has one per architecture
// — so asking for the image without saying which one is asking the wrong
// question. go-containerregistry answers linux/amd64 when nothing says
// otherwise, which is how an Apple Silicon Mac came to pin the amd64 digest of
// a harness image its arm64 pool would never hold: the sandbox refused to
// launch, reporting that the tag "now resolves to" a digest that had never
// been anything else.
//
// The control plane's own architecture, and linux regardless of the control
// plane's OS: the pool is a Linux machine, and on every provider that runs one
// on this host — vz, wslc, libkrun, docker — it is this machine's architecture.
// A cloud pool of a different architecture is not answered by this, and cannot
// be by any single digest recorded once per harness config.
func poolPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
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
	metadata, err := parseImageMetadata(localImageDigest(inspected), labels)
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
