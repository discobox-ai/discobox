package harnessconfigs

import (
	"context"
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

// parseImageMetadata resolves an image's label set into the one manifest it
// effectively declares: every inherited layer, then the image's own (ADR 0086
// §2). Only the merged result is validated — a layer on its own is a fragment,
// and the base layer legitimately carries no harness at all.
func parseImageMetadata(digest string, labels map[string]string) (imageMetadata, error) {
	metadata, hasBase, err := harness.ResolveImageLabels(labels)
	if err != nil {
		return imageMetadata{}, err
	}
	// The base layer is the only evidence of lineage available without pulling
	// filesystem layers, and lineage is a hard requirement: the runtime
	// contract (PID 1, systemd units, the runc wrapper) lives in the base
	// image's filesystem, so an image that did not come from it cannot run a
	// sandbox whatever it declares (ADR 0086 §1). Saying that beats reporting a
	// missing label, which is the same fact phrased as a paperwork error.
	if !hasBase {
		return imageMetadata{}, fmt.Errorf("image is not built FROM discobox-sandbox-agent: it carries no %s%s label", harness.ImageLayerLabelPrefix, harness.SandboxBaseLayer)
	}
	if err := validateImageMetadata(metadata); err != nil {
		return imageMetadata{}, err
	}
	return imageMetadata{Digest: digest, ImageMetadata: metadata}, nil
}

func validateImageMetadata(metadata harness.ImageMetadata) error {
	// A manifest is optional in full: an image that installs its agent as
	// harness.RunCommand and needs no credentials declares nothing, and takes
	// its identity from the registration (ADR 0086 §5). What remains here are
	// the rules about what a *present* declaration may say.
	h := metadata.Harness
	if h == nil {
		h = &harness.Image{}
	}
	// An omitted runCommand is a declaration, not an omission: it means the
	// image installs the conventional harness.RunCommand, which the runtime
	// types (ADR 0086 §3). A *present but blank* command is still a broken
	// image.
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

// devImageInspector resolves harness metadata from the development image
// manifest before falling back to a daemon or registry lookup.
//
// In build-mode the host has no Docker daemon and the dev image has never been
// pushed, so neither fallback can see it: the image exists only as a build
// description until some pool's daemon builds it. The manifest already carries
// the metadata verbatim, as the build arguments that become the labels, so
// seeding reads it from there and no longer depends on the image existing yet.
//
// It reconstructs the label set the built image would carry, inherited layers
// included, by walking the same edge the manifest already uses to order builds:
// a harness entry's SANDBOX_AGENT_IMAGE argument names the base entry's
// reference, whose own layer argument is the layer that image would label
// (ADR 0086 §4). Without that walk a developer on Windows or macOS would
// resolve a manifest missing every inherited volume and env var, and every
// harness image would be rejected as not built from the base.
type devImageInspector struct {
	labelsByReference map[string]map[string]string
	fallback          imageInspector
}

// newDevImageInspector wraps fallback with the build-mode manifest entries in
// images. It returns fallback unchanged when no entry carries image metadata,
// so copy-mode and production keep the original behavior exactly.
func newDevImageInspector(images []devimage.Image, fallback imageInspector) imageInspector {
	// The layer each entry contributes to whatever is built FROM it, keyed by
	// the reference a child names it under.
	layerByReference := map[string]string{}
	for _, image := range images {
		if image.Build == nil {
			continue
		}
		if raw := strings.TrimSpace(image.Build.Args[harness.LayerMetadataBuildArg]); raw != "" {
			layerByReference[strings.TrimSpace(image.Reference)] = raw
		}
	}

	labels := map[string]map[string]string{}
	for _, image := range images {
		if image.Build == nil {
			continue
		}
		reference := strings.TrimSpace(image.Reference)
		own := strings.TrimSpace(image.Build.Args[harness.MetadataBuildArg])
		parent := strings.TrimSpace(image.Build.Args[sandboxAgentImageBuildArg])
		inherited, hasParent := layerByReference[parent]
		if own == "" && !hasParent {
			continue
		}
		entry := map[string]string{}
		if hasParent {
			entry[harness.ImageLayerLabelPrefix+harness.SandboxBaseLayer] = inherited
		}
		// An image that declares nothing sets no label of its own, exactly as
		// its Dockerfile does not.
		if own != "" {
			entry[harness.ImageLabel] = own
		}
		labels[reference] = entry
	}
	if len(labels) == 0 {
		return fallback
	}
	return devImageInspector{labelsByReference: labels, fallback: fallback}
}

// sandboxAgentImageBuildArg is the build argument a harness Dockerfile takes
// its base image reference in, and so the manifest edge from a harness image to
// the base whose layer it inherits. Keep in sync with the harness Dockerfiles.
const sandboxAgentImageBuildArg = "SANDBOX_AGENT_IMAGE"

func (d devImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	labels, ok := d.labelsByReference[strings.TrimSpace(imageRef)]
	if !ok {
		return d.fallback.Inspect(ctx, imageRef)
	}
	// A build-mode reference is content-addressed over that image's inputs, so
	// it is its own freshness key; there is no digest until it is built.
	return parseImageMetadata(imageRef, labels)
}
