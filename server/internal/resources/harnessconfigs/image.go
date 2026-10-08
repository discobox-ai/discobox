package harnessconfigs

import (
	"context"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	dockerclient "github.com/moby/moby/client"
)

type imageMetadata struct {
	Digest string
	// Platforms is what the image is published for (ADR 0145 §1): every
	// platform in a registry image's index, or the one platform a locally built
	// image was built for. Empty when nothing says, which rules nothing out.
	Platforms platform.Set
	harness.ImageMetadata
}

// imageInspector reads a harness image's manifest, the digest to pin it to, and
// the platforms it is published for.
type imageInspector interface {
	Inspect(ctx context.Context, imageRef string) (imageMetadata, error)
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
		remote.WithAuthFromKeychain(registryauth.Keychain()),
		// The platform whose labels are read when the image publishes it. Always
		// named: go-containerregistry answers linux/amd64 when nothing says
		// otherwise, and an image published only for arm64 then reads as one
		// that does not exist.
		remote.WithPlatform(v1.Platform{OS: platform.Pool().OS, Architecture: platform.Pool().Arch}),
	}
	// One GET, because it answers every question: the descriptor carries the
	// digest the registry serves this tag under and, for a multi-platform
	// image, the index that lists what it is published for.
	//
	// Not remote.Head, which asks for exactly the digest and nothing else:
	// ghcr.io answers HEAD without a Content-Length header, which
	// go-containerregistry rejects — so every harness image "was unavailable",
	// none were seeded, and a project came up with no harnesses at all.
	descriptor, err := remote.Get(ref, remoteOptions...)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("inspect harness image %q: %w", imageRef, registryauth.Explain(ref, err))
	}
	image, platforms, err := publishedImage(descriptor)
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
	// Not the config digest. A local Docker daemon reports that as an image ID
	// only under the classic image store; the containerd one, the default in
	// current Docker, reports the index digest. A recorded config digest is
	// therefore a value the daemon never produces, and every sandbox on a
	// published multi-arch image would refuse to launch: "pinned to
	// sha256:6a5066…, now resolves to sha256:4a5726…" — the config digest and
	// the index digest of one unchanged image.
	//
	// Both store types put this value in RepoDigests, which is what the pool
	// compares against, so one recorded digest works on either. An index digest
	// is also what lets one pin serve a pool of each platform the index lists.
	metadata, err := parseImageMetadata(descriptor.Digest.String(), config.Config.Labels)
	if err != nil {
		return imageMetadata{}, err
	}
	if platforms == nil {
		// A single-platform image says what it is in its config.
		platforms = platform.NewSet(platform.Platform{OS: config.OS, Arch: config.Architecture})
	}
	metadata.Platforms = platforms
	return metadata, nil
}

// publishedImage resolves the image whose labels a harness is read from, and,
// for a multi-platform image, every platform its index publishes. The labels
// are read from the platform a pool on this machine hosts when the index lists
// it, and from the first platform it lists otherwise: an image published only
// for another architecture still declares the same harness.
//
// platforms is nil for a single-platform image, whose config says what it is.
func publishedImage(descriptor *remote.Descriptor) (v1.Image, platform.Set, error) {
	if !descriptor.MediaType.IsIndex() {
		image, err := descriptor.Image()
		return image, nil, err
	}
	index, err := descriptor.ImageIndex()
	if err != nil {
		return nil, nil, err
	}
	platforms, digests, err := indexPlatforms(index)
	if err != nil {
		return nil, nil, err
	}
	if platforms.Contains(platform.Pool()) {
		image, err := descriptor.Image()
		return image, platforms, err
	}
	image, err := index.Image(digests[platforms[0]])
	return image, platforms, err
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

// indexPlatforms is every platform an index publishes, attestation entries
// aside, and the digest of the first manifest it lists for each.
func indexPlatforms(index v1.ImageIndex) (platform.Set, map[platform.Platform]v1.Hash, error) {
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, nil, err
	}
	listed := make([]platform.Platform, 0, len(manifest.Manifests))
	digests := map[platform.Platform]v1.Hash{}
	for _, entry := range manifest.Manifests {
		if entry.Platform == nil {
			continue
		}
		p := platform.Platform{OS: entry.Platform.OS, Arch: entry.Platform.Architecture}
		listed = append(listed, p)
		if _, ok := digests[p]; !ok {
			digests[p] = entry.Digest
		}
	}
	platforms := platform.NewSet(listed...)
	if len(platforms) == 0 {
		return nil, nil, fmt.Errorf("its index lists no platform")
	}
	return platforms, digests, nil
}

// registryPlatforms is what a pulled image is published for, asked of the
// registry it was pulled from by the digest the daemon recorded. A daemon
// reports a pulled image as its own platform even when the registry publishes
// several, and recording that would refuse a pool of another architecture an
// image that runs there. A registry that cannot be asked answers nil — no
// platforms read, which rules nothing out — rather than the daemon's one.
func registryPlatforms(ctx context.Context, repoDigest string) platform.Set {
	ref, err := name.ParseReference(repoDigest)
	if err != nil {
		return nil
	}
	descriptor, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(registryauth.Keychain()))
	if err != nil {
		return nil
	}
	if descriptor.MediaType.IsIndex() {
		index, err := descriptor.ImageIndex()
		if err != nil {
			return nil
		}
		platforms, _, err := indexPlatforms(index)
		if err != nil {
			return nil
		}
		return platforms
	}
	image, err := descriptor.Image()
	if err != nil {
		return nil
	}
	config, err := image.ConfigFile()
	if err != nil {
		return nil
	}
	return platform.NewSet(platform.Platform{OS: config.OS, Arch: config.Architecture})
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
	if err != nil {
		return metadata, true, err
	}
	if len(inspected.RepoDigests) > 0 {
		// Pulled from a registry, which says what it is published for.
		metadata.Platforms = registryPlatforms(ctx, inspected.RepoDigests[0])
	} else {
		// Built here, which is what a development build does: it is the one
		// platform it was built for, and runs only on a pool of that platform.
		metadata.Platforms = platform.NewSet(platform.Platform{OS: inspected.Os, Arch: inspected.Architecture})
	}
	return metadata, true, nil
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
	if h.Config != nil {
		if len(h.Config.Command) == 0 || strings.TrimSpace(h.Config.Command[0]) == "" {
			return fmt.Errorf("%s label config mode requires command", harness.ImageLabel)
		}
		// A declared port is forwarded at its own number or not at all, so a
		// number no listener can hold is a broken image rather than a forward
		// that quietly lands elsewhere. Duplicates are rejected for the same
		// reason: the second declaration could only ever contradict the first
		// about what to say when the port is unavailable.
		ports := map[int]struct{}{}
		for _, port := range h.Config.Ports {
			if port.Port < 1 || port.Port > 65535 {
				return fmt.Errorf("%s label config port %d is out of range", harness.ImageLabel, port.Port)
			}
			if _, ok := ports[port.Port]; ok {
				return fmt.Errorf("%s label has duplicate config port %d", harness.ImageLabel, port.Port)
			}
			ports[port.Port] = struct{}{}
		}
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
		if err := harness.ValidateVolume(volume); err != nil {
			return fmt.Errorf("%s label volume %q: %w", harness.ImageLabel, volume.Path, err)
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
// seeding reads it from there and does not depend on the image existing yet.
//
// It reconstructs the label set the built image would carry, inherited layers
// included, by walking the same edge the manifest already uses to order builds:
// a harness entry's SANDBOX_AGENT_IMAGE argument names the base entry's
// reference, whose own layer argument is the layer that image would label
// (ADR 0086 §2). Without that walk a developer on Windows or macOS would
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
	// it is its own freshness key; there is no digest until it is built. Nor
	// is there a platform: the pool that runs it builds it for its own, so the
	// set is left empty and rules no pool out.
	return parseImageMetadata(imageRef, labels)
}
