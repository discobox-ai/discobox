package harnessconfigs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxuser"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	imagetypes "github.com/moby/moby/api/types/image"
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

type defaultImageInspector struct {
	// overlayDir is the one directory a file:// manifest reference may name
	// a file in (inspectManifestFile).
	overlayDir string
}

func (d defaultImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	// A template with no image — a non-Linux one, assembled from a vendor's
	// base and Discobox's overlay — is named by its overlay's manifest file
	// (ADR 0145 §3), and is never a reference a daemon or a registry knows.
	if strings.HasPrefix(strings.TrimSpace(imageRef), manifestFileScheme) {
		return inspectManifestFile(imageRef, d.overlayDir)
	}
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
	// The manifest list, which a daemon on the containerd image store reports
	// from API 1.48 on, is what says which platforms the image is published
	// for. A daemon that cannot report it is asked again without it.
	inspected, err := client.ImageInspect(ctx, imageRef, dockerclient.ImageInspectWithManifests(true))
	if err != nil {
		inspected, err = client.ImageInspect(ctx, imageRef)
	}
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
	metadata.Platforms = localPlatforms(inspected.InspectResponse)
	if metadata.Platforms == nil {
		metadata.Platforms = localPlatformsWithoutManifests(ctx, inspected.InspectResponse)
	}
	return metadata, true, nil
}

// localPlatforms is what a local image is published for, from the manifest
// list its daemon reports: every image manifest in its index, whether or not
// this daemon holds that platform's content. A pulled multi-platform image
// lists them all; an image built here lists the one platform it was built for,
// which is what a development build makes, and which runs only on a pool of
// that platform. Attestation manifests are not platforms. nil when the daemon
// reported no manifest list.
func localPlatforms(inspected imagetypes.InspectResponse) platform.Set {
	listed := make([]platform.Platform, 0, len(inspected.Manifests))
	for _, manifest := range inspected.Manifests {
		if manifest.Kind != imagetypes.ManifestKindImage || manifest.ImageData == nil {
			continue
		}
		listed = append(listed, platform.Platform{OS: manifest.ImageData.Platform.OS, Arch: manifest.ImageData.Platform.Architecture})
	}
	if len(listed) == 0 {
		return nil
	}
	return platform.NewSet(listed...)
}

// localPlatformsWithoutManifests answers for a daemon that reports no manifest
// list — the classic image store, or one older than API 1.48. On the classic
// store an image with a registry digest was pulled, and the registry says what
// it is published for; one without was built here, and is the one platform it
// was built for. The daemon's own platform is never taken for an image with a
// registry digest: it reports a pulled multi-platform image as the one
// platform it holds. A containerd-store daemon older than API 1.48 gives a
// local build a registry digest too; its registry has never heard of it, and
// the set is left unread — which rules nothing out, so such a build on a pool
// of another platform fails on that pool rather than at placement.
func localPlatformsWithoutManifests(ctx context.Context, inspected imagetypes.InspectResponse) platform.Set {
	if len(inspected.RepoDigests) > 0 {
		return registryPlatforms(ctx, inspected.RepoDigests[0])
	}
	return platform.NewSet(platform.Platform{OS: inspected.Os, Arch: inspected.Architecture})
}

// manifestFileScheme prefixes a harness reference that names a manifest file
// rather than an image: file:// and an absolute path on this server's host,
// inside its overlay directory.
const manifestFileScheme = "file://"

// inspectManifestFile reads a manifest file named by a file:// reference: the
// same layers an image carries in its labels, resolved the same way. Its
// digest is the file's own, so a changed overlay is a moved pin exactly as a
// rebuilt tag is (ADR 0145 §2), and its one platform is the one it declares —
// there is no registry to say what else it is published for.
//
// The file must be in overlayDir, where overlays are staged, and it is read
// through an os.Root on that directory, so neither `..` nor a symlink leaves
// it. A reference is a harness registration's, which any project member
// makes over the API: read from anywhere, it would have this server open a
// path the caller chose and answer with whether it exists and what it holds.
// A reference outside is refused before anything is opened, and the refusal
// names only the directory.
func inspectManifestFile(ref, overlayDir string) (imageMetadata, error) {
	parsed, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return imageMetadata{}, fmt.Errorf("parse manifest file reference %q: %w", ref, err)
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return imageMetadata{}, fmt.Errorf("manifest file reference %q names host %q: a manifest file is read from this server's own disk", ref, parsed.Host)
	}
	path := filepath.FromSlash(parsed.Path)
	if runtime.GOOS == "windows" {
		// file:///C:/x parses to the path /C:/x.
		path = filepath.FromSlash(strings.TrimPrefix(parsed.Path, "/"))
	}
	if !filepath.IsAbs(path) {
		return imageMetadata{}, fmt.Errorf("manifest file reference %q must name an absolute path", ref)
	}
	overlayDir = strings.TrimSpace(overlayDir)
	if overlayDir == "" {
		return imageMetadata{}, fmt.Errorf("manifest file reference %q: this server has no overlay directory to read manifest files from", ref)
	}
	rel, err := filepath.Rel(filepath.Clean(overlayDir), filepath.Clean(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return imageMetadata{}, fmt.Errorf("manifest file reference %q is not in the overlay directory %s: a manifest file is read from there and nowhere else", ref, overlayDir)
	}
	root, err := os.OpenRoot(overlayDir)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("open overlay directory: %w", err)
	}
	defer root.Close()
	data, err := root.ReadFile(rel)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read manifest file %q: %w", ref, err)
	}
	sum := sha256.Sum256(data)
	return parseManifestFile("sha256:"+hex.EncodeToString(sum[:]), data)
}

// manifestFileSource is what a manifest file's errors say they are about.
const manifestFileSource = "manifest file"

// imageLabelSource is what an image's label errors say they are about.
const imageLabelSource = harness.ImageLabel + " label"

// parseManifestFile resolves a manifest file's layers and validates the result
// for the platform it declares, which a manifest file must: nothing else says
// what its template runs. That platform is never Linux, whose templates are
// images: a Linux pool runs the reference it is handed as a container image,
// and a file:// one would fail there at create rather than here.
func parseManifestFile(digest string, data []byte) (imageMetadata, error) {
	labels, err := harness.ReadManifestFile(data)
	if err != nil {
		return imageMetadata{}, err
	}
	metadata, hasBase, err := harness.ResolveImageLabels(labels)
	if err != nil {
		return imageMetadata{}, err
	}
	// The base layer is the sandbox agent's for that platform, shipped in the
	// same overlay as the agent, and proves lineage just as an image's does.
	if !hasBase {
		return imageMetadata{}, fmt.Errorf("manifest file carries no %s%s layer: its template is not built on the sandbox agent's overlay", harness.ImageLayerLabelPrefix, harness.SandboxBaseLayer)
	}
	if metadata.Platform.IsZero() {
		return imageMetadata{}, fmt.Errorf("manifest file declares no platform: a template with no image says which one it runs")
	}
	if sandboxuser.HasPOSIXIDs(metadata.Platform.OS) {
		return imageMetadata{}, fmt.Errorf("manifest file declares platform %s: a %s template is an image, and a manifest file is for a template with none", metadata.Platform, metadata.Platform.OS)
	}
	if err := validateImageMetadata(metadata, metadata.Platform.OS, manifestFileSource); err != nil {
		return imageMetadata{}, err
	}
	return imageMetadata{Digest: digest, Platforms: platform.NewSet(metadata.Platform), ImageMetadata: metadata}, nil
}

// parseImageMetadata resolves an image's label set into the one manifest it
// effectively declares: every inherited layer, then the image's own (ADR 0086
// §2). Only the merged result is validated — a layer on its own is a fragment,
// and the base layer legitimately carries no harness at all.
//
// An image is a Linux container's, and its platforms are what its registry
// publishes, so a label naming a platform is refused rather than read beside
// that answer.
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
	if !metadata.Platform.IsZero() {
		return imageMetadata{}, fmt.Errorf("%s label declares platform %s: an image's platforms are the ones its registry publishes it for", harness.ImageLabel, metadata.Platform)
	}
	if err := validateImageMetadata(metadata, "linux", imageLabelSource); err != nil {
		return imageMetadata{}, err
	}
	return imageMetadata{Digest: digest, ImageMetadata: metadata}, nil
}

// validateImageMetadata judges a resolved manifest for a sandbox whose
// platform's OS is goos. source is what an error says it is about: an image's
// label, or a manifest file.
func validateImageMetadata(metadata harness.ImageMetadata, goos, source string) error {
	if err := metadata.ValidateFor(goos); err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
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
		return fmt.Errorf("%s has a blank runCommand", source)
	}
	if h.Config != nil {
		if len(h.Config.Command) == 0 || strings.TrimSpace(h.Config.Command[0]) == "" {
			return fmt.Errorf("%s config mode requires command", source)
		}
		// A declared port is forwarded at its own number or not at all, so a
		// number no listener can hold is a broken image rather than a forward
		// that quietly lands elsewhere. Duplicates are rejected for the same
		// reason: the second declaration could only ever contradict the first
		// about what to say when the port is unavailable.
		ports := map[int]struct{}{}
		for _, port := range h.Config.Ports {
			if port.Port < 1 || port.Port > 65535 {
				return fmt.Errorf("%s config port %d is out of range", source, port.Port)
			}
			if _, ok := ports[port.Port]; ok {
				return fmt.Errorf("%s has duplicate config port %d", source, port.Port)
			}
			ports[port.Port] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	for _, secret := range h.Secrets {
		name := strings.TrimSpace(secret.Name)
		if !services.HarnessConfigEnvVarNamePattern.MatchString(name) {
			return fmt.Errorf("%s has invalid secret environment variable %q", source, secret.Name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s has duplicate secret %q", source, name)
		}
		seen[name] = struct{}{}
	}
	for idx, volume := range metadata.Volumes {
		if strings.TrimSpace(volume.Path) == "" {
			return fmt.Errorf("%s volume[%d] requires path", source, idx)
		}
		if err := harness.ValidateVolume(volume); err != nil {
			return fmt.Errorf("%s volume %q: %w", source, volume.Path, err)
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
