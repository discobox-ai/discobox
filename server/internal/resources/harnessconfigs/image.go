package harnessconfigs

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/secretroute"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	imagetypes "github.com/moby/moby/api/types/image"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type imageMetadata struct {
	// Digest is the runnable image's, or a manifest file's: what a sandbox
	// pins. BundleDigest is the bundle's the manifest was read from, empty for
	// a manifest file, which is no bundle.
	Digest       string
	BundleDigest string
	// Platforms is what the image is published for (ADR 0145 §1): every
	// platform in a registry image's index, or the one platform a locally built
	// image was built for. Empty when nothing says, which rules nothing out.
	Platforms platform.Set
	// ImageMetadata is the resolved manifest. Its ImageKind is the kind the
	// harness's image is (ADR 26-10-09-106 §4), always set once inspected: OCI
	// for an image, or the disco-vm kind a manifest file declares.
	harness.ImageMetadata
}

// bundleInspection is what a bundle is read for (ADR 26-10-16-423 §2): the
// digest to pin it to, and the labels its manifest is carried in.
type bundleInspection struct {
	Digest string
	Labels map[string]string
}

// imageInspector reads what a harness config is snapshotted from.
//
// Inspect reads a harness's runnable image — the digest a sandbox pins and the
// platforms it is published for, never its labels, which describe the image
// it was built from rather than the harness — or a manifest file, whose
// layers are the whole manifest of a template with no image. InspectBundle
// reads a bundle, refusing one carrying a whiteout: a bundle's layers add
// feature directories side by side, and remove nothing (ADR 26-10-16-423 §2).
// admit judges its labels before any layer is read, so a reference that is
// not the bundle it is meant to be — a runnable image of several GB named as
// a harness's bundle — is refused for what its labels say, not after its
// layers are saved and read.
type imageInspector interface {
	Inspect(ctx context.Context, imageRef string) (imageMetadata, error)
	InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error)
}

type defaultImageInspector struct {
	// overlayDir is the one directory a file:// manifest reference may name
	// a file in (inspectManifestFile).
	overlayDir string
	// verified is the bundles whose layers were read and found to carry no
	// whiteout, by digest, so each is read once rather than on every seed.
	// Nil reads every time.
	verified *verifiedBundles
}

// verifiedBundles is the digests whose layers were found to carry no
// whiteout. The verdict is the content's, which its digest names, so it never
// goes stale: a bundle's layers are read in full — the platform bundle's are
// the agent's binaries — once per digest while the server runs, not on every
// seeding pass, which each project's ensure runs.
type verifiedBundles struct {
	mu      sync.Mutex
	digests map[string]bool
}

func (v *verifiedBundles) has(digest string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.digests[digest]
}

func (v *verifiedBundles) add(digest string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.digests == nil {
		v.digests = map[string]bool{}
	}
	v.digests[digest] = true
}

func (d defaultImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	// A template with no image — a non-Linux one, assembled from a vendor's
	// base and Discobox's overlay — is named by its overlay's manifest file
	// (ADR 0145 §3), and is never a reference a daemon or a registry knows.
	if isManifestFile(imageRef) {
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
	metadata := runnableImage(descriptor.Digest.String())
	if platforms == nil {
		// A single-platform image says what it is in its config.
		config, err := image.ConfigFile()
		if err != nil {
			return imageMetadata{}, fmt.Errorf("read harness image config %q: %w", imageRef, err)
		}
		platforms = platform.NewSet(platform.Platform{OS: config.OS, Arch: config.Architecture})
	}
	metadata.Platforms = platforms
	return metadata, nil
}

// runnableImage is what a harness's runnable image is inspected for: the digest
// a sandbox pins, and its kind, OCI by its being an image. Its manifest is its
// bundle's (resolveHarness).
func runnableImage(digest string) imageMetadata {
	return imageMetadata{Digest: digest, ImageMetadata: harness.ImageMetadata{ImageKind: platform.OCI}}
}

func (d defaultImageInspector) InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error) {
	if isManifestFile(bundleRef) {
		return bundleInspection{}, fmt.Errorf("bundle %q names a manifest file: a bundle is an OCI image", bundleRef)
	}
	// A locally present bundle is authoritative once found, as an image is
	// (Inspect).
	if local, found, err := inspectLocalBundle(ctx, bundleRef, admit, d.verified); found {
		return local, err
	}
	ref, err := name.ParseReference(bundleRef)
	if err != nil {
		return bundleInspection{}, fmt.Errorf("parse bundle %q: %w", bundleRef, err)
	}
	descriptor, err := remote.Get(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(registryauth.Keychain()),
		remote.WithPlatform(v1.Platform{OS: platform.Pool().OS, Architecture: platform.Pool().Arch}))
	if err != nil {
		return bundleInspection{}, fmt.Errorf("inspect bundle %q: %w", bundleRef, registryauth.Explain(ref, err))
	}
	// The platform bundle is an index; a harness bundle is one manifest.
	image, _, err := publishedImage(descriptor)
	if err != nil {
		return bundleInspection{}, fmt.Errorf("resolve bundle %q: %w", bundleRef, err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return bundleInspection{}, fmt.Errorf("read bundle config %q: %w", bundleRef, err)
	}
	if err := admitBundle(config.Config.Labels, admit); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q: %w", bundleRef, err)
	}
	digest := descriptor.Digest.String()
	if err := verifyBundle(bundleRef, digest, image.Layers, d.verified); err != nil {
		return bundleInspection{}, err
	}
	return bundleInspection{Digest: digest, Labels: config.Config.Labels}, nil
}

// admitBundle judges a bundle's labels, before any layer is read: first that
// it is a bundle at all, then what admit asks of the kind it is meant to be.
//
// A bundle is FROM scratch, so it carries no label it did not set, and every
// runnable image carries the ReclaimLabel discobox-sandbox-base sets: the sandbox
// agent image, every harness twin, anything built FROM them. That is the one
// label to tell them apart by, since the runnable images carry the manifest
// labels too, to move their digests with it — which would admit the sandbox
// agent image as the platform bundle, and have its layers saved and read.
func admitBundle(labels map[string]string, admit func(labels map[string]string) error) error {
	if _, runnable := labels[harness.ReclaimLabel]; runnable {
		return fmt.Errorf("it carries %s, so it is a runnable image built FROM discobox-sandbox-base, and a bundle is built FROM scratch", harness.ReclaimLabel)
	}
	return admit(labels)
}

// verifyBundle refuses a bundle when a layer carries a whiteout, reading its
// layers only when its digest has not been read before.
func verifyBundle(bundleRef, digest string, layers func() ([]v1.Layer, error), verified *verifiedBundles) error {
	if verified.has(digest) {
		return nil
	}
	if err := refuseWhiteouts(layers); err != nil {
		return fmt.Errorf("bundle %q: %w", bundleRef, err)
	}
	verified.add(digest)
	return nil
}

// refuseWhiteouts refuses a bundle any of whose layers carries a whiteout, the
// entry by which a layer deletes what one beneath it holds. A bundle's tree is
// its layers side by side, never merged as an overlay (ADR 26-10-16-423 §13),
// so a deletion would mean nothing there but what the layer beneath says.
func refuseWhiteouts(read func() ([]v1.Layer, error)) error {
	layers, err := read()
	if err != nil {
		return fmt.Errorf("read layers: %w", err)
	}
	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			return fmt.Errorf("read layer digest: %w", err)
		}
		if err := refuseLayerWhiteouts(layer); err != nil {
			return fmt.Errorf("layer %s: %w", digest, err)
		}
	}
	return nil
}

func refuseLayerWhiteouts(layer v1.Layer) error {
	contents, err := layer.Uncompressed()
	if err != nil {
		return err
	}
	defer contents.Close()
	entries := tar.NewReader(contents)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(path.Base(header.Name), whiteoutPrefix) {
			return fmt.Errorf("carries the whiteout %q: a bundle's layers add feature directories and delete nothing", header.Name)
		}
	}
}

// whiteoutPrefix starts the name of every whiteout entry in an OCI layer, the
// opaque marker included.
const whiteoutPrefix = ".wh."

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
// is, and it is also all the pool will have to compare against. A containerd
// image store makes that ID the image's manifest digest; the classic store
// makes it its config's.
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
// only when the daemon returned an image, so the caller falls back to the
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
	metadata := runnableImage(localImageDigest(inspected))
	metadata.Platforms = localPlatforms(inspected.InspectResponse)
	if metadata.Platforms == nil {
		metadata.Platforms = localPlatformsWithoutManifests(ctx, inspected.InspectResponse)
	}
	return metadata, true, nil
}

// inspectLocalBundle inspects bundleRef via the local Docker daemon, which
// holds it when it was built or staged here. found is true once the daemon
// returned the bundle, so an error reading what it holds is reported rather
// than masked by a registry fetch of the same (often :local) reference.
//
// Its layers are read from what the daemon saves, and only for a digest not
// read before (verified).
func inspectLocalBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error, verified *verifiedBundles) (bundleInspection, bool, error) {
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return bundleInspection{}, false, err
	}
	defer client.Close()
	inspected, err := client.ImageInspect(ctx, bundleRef)
	if err != nil {
		return bundleInspection{}, false, err
	}
	read := bundleInspection{Digest: localImageDigest(inspected)}
	if inspected.Config != nil {
		read.Labels = inspected.Config.Labels
	}
	if err := admitBundle(read.Labels, admit); err != nil {
		return bundleInspection{}, true, fmt.Errorf("bundle %q: %w", bundleRef, err)
	}
	if verified.has(read.Digest) {
		return read, true, nil
	}
	image, cleanup, err := saveLocalImage(ctx, client, bundleRef, ocispec.Platform{OS: inspected.Os, Architecture: inspected.Architecture})
	if err != nil {
		return bundleInspection{}, true, fmt.Errorf("save bundle %q: %w", bundleRef, err)
	}
	defer cleanup()
	return read, true, verifyBundle(bundleRef, read.Digest, image.Layers, verified)
}

// saveLocalImage saves ref from the daemon to a file, read as a docker save
// tarball, which reads it more than once. It asks for the daemon's own
// platform: a pulled platform bundle is an index this daemon may hold one
// platform of, which a containerd image store refuses to save whole. A daemon
// older than API 1.48 cannot be asked for a platform, and is asked for the
// image as it holds it.
func saveLocalImage(ctx context.Context, client *dockerclient.Client, ref string, own ocispec.Platform) (v1.Image, func(), error) {
	saved, err := client.ImageSave(ctx, []string{ref}, dockerclient.ImageSaveWithPlatforms(own))
	if err != nil {
		saved, err = client.ImageSave(ctx, []string{ref})
	}
	if err != nil {
		return nil, nil, err
	}
	defer saved.Close()
	file, err := os.CreateTemp("", "discobox-bundle-*.tar")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if _, err := io.Copy(file, saved); err != nil {
		cleanup()
		return nil, nil, err
	}
	image, err := tarball.ImageFromPath(file.Name(), nil)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read the saved image: %w", err)
	}
	return image, cleanup, nil
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

// isManifestFile reports whether ref names a manifest file.
func isManifestFile(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), manifestFileScheme)
}

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
	// A configured directory may be relative, and a reference never is.
	overlayDir, err = filepath.Abs(overlayDir)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("resolve overlay directory: %w", err)
	}
	rel, err := filepath.Rel(overlayDir, filepath.Clean(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return imageMetadata{}, fmt.Errorf("manifest file reference %q is not in the overlay directory %s: a manifest file is read from there and nowhere else", ref, overlayDir)
	}
	root, err := os.OpenRoot(overlayDir)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("open overlay directory: %w", err)
	}
	defer root.Close()
	data, err := readManifestFile(root, rel)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read manifest file %q: %w", ref, err)
	}
	sum := sha256.Sum256(data)
	return parseManifestFile("sha256:"+hex.EncodeToString(sum[:]), data)
}

// maxManifestFileBytes bounds what a manifest file read buffers. An overlay
// also holds the agent's binaries, and a reference to one of those must not
// have the server read a binary whole into memory, once per request, to learn
// it is not JSON. A manifest is a few kilobytes.
const maxManifestFileBytes = 1 << 20

// readManifestFile reads the regular file at rel under root, refusing one
// larger than maxManifestFileBytes before buffering it.
func readManifestFile(root *os.Root, rel string) ([]byte, error) {
	file, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestFileBytes {
		return nil, fmt.Errorf("larger than %d bytes, which no manifest is", maxManifestFileBytes)
	}
	return data, nil
}

// manifestFileSource is what a manifest file's errors say they are about.
const manifestFileSource = "manifest file"

// parseManifestFile resolves a manifest file's layers and validates the result
// for the platform it declares, which a manifest file must: nothing else says
// what its template runs. It must declare its image kind too, and that kind is
// a disco-vm image for a driver (ADR 26-10-09-106 §4): an OCI template is an
// image, and a reference to a file is none. The kind is what keeps a Linux
// manifest file off a Docker pool of its platform — placement refuses it there
// with that reason — so a manifest file may be any platform's.
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
	if metadata.ImageKind.Format != platform.FormatDiscoVM {
		return imageMetadata{}, fmt.Errorf("manifest file declares no disco-vm image kind: a template with no image names the disco-vm driver its image is built for, as %s/<driver>", platform.FormatDiscoVM)
	}
	if err := validateImageMetadata(metadata, metadata.Platform.OS, manifestFileSource); err != nil {
		return imageMetadata{}, err
	}
	return imageMetadata{Digest: digest, Platforms: platform.NewSet(metadata.Platform), ImageMetadata: metadata}, nil
}

// composeBundleMetadata resolves a harness bundle's labels, composed on the
// platform bundle's (harness.ComposeBundleLabels), into the one manifest the
// harness effectively declares. Only the merged result is validated.
//
// A bundle is a Linux sandbox's, and names neither a platform nor an image
// kind: its steps select on the platform they run on, and the kind is the
// provider's (ADR 26-10-16-423 §2). The kind is OCI here, the one the
// runnable image a pool runs it as is.
func composeBundleMetadata(platformBundle, harnessBundle bundleInspection) (harness.ImageMetadata, error) {
	metadata, err := harness.ComposeBundleLabels(platformBundle.Labels, harnessBundle.Labels)
	if err != nil {
		return harness.ImageMetadata{}, err
	}
	if !metadata.Platform.IsZero() {
		return harness.ImageMetadata{}, fmt.Errorf("%s declares platform %s: a bundle's steps select the platform they run on", bundleLabelSource, metadata.Platform)
	}
	if !metadata.ImageKind.IsZero() {
		return harness.ImageMetadata{}, fmt.Errorf("%s declares image kind %s: a bundle names no image kind, which is the provider's", bundleLabelSource, metadata.ImageKind)
	}
	if err := validateImageMetadata(metadata, "linux", bundleLabelSource); err != nil {
		return harness.ImageMetadata{}, err
	}
	metadata.ImageKind = platform.OCI
	return metadata, nil
}

const bundleLabelSource = "bundle's " + harness.ImageLabel + " label"

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
		// about what to say when the port is unavailable. An ephemeral entry
		// names no number — the sandbox's listening ports supply them — and
		// there is one at most, for the same reason.
		ports := map[int]struct{}{}
		ephemeral := false
		for _, port := range h.Config.Ports {
			if port.Ephemeral {
				if port.Port != 0 {
					return fmt.Errorf("%s ephemeral config port also names port %d", source, port.Port)
				}
				if ephemeral {
					return fmt.Errorf("%s has duplicate ephemeral config port", source)
				}
				ephemeral = true
				continue
			}
			if port.Port < 1 || port.Port > 65535 {
				return fmt.Errorf("%s config port %d is out of range", source, port.Port)
			}
			if _, ok := ports[port.Port]; ok {
				return fmt.Errorf("%s has duplicate config port %d", source, port.Port)
			}
			ports[port.Port] = struct{}{}
		}
	}
	// A host the proxy could not match would be a harness that never reaches
	// what it said it needs once a level restricts it, failing far from here.
	if err := (egress.Policy{Allow: h.EgressHosts}).Validate(); err != nil {
		return fmt.Errorf("%s label egressHosts: %w", harness.ImageLabel, err)
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
		for _, rule := range secret.RejectionScope {
			if err := rule.Validate(); err != nil {
				return fmt.Errorf("%s secret %q: %w", source, name, err)
			}
		}
		for _, route := range secret.Routes {
			if _, _, err := secretroute.Normalize(route.Host, route.Path); err != nil {
				return fmt.Errorf("%s secret %q declares a bad route: %w", source, name, err)
			}
		}
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

// devImageInspector answers for the development image manifest's entries
// before falling back to a daemon or registry lookup.
//
// In build-mode the host has no Docker daemon and the dev images have never
// been pushed, so neither fallback can see them: each exists only as a build
// description until some pool's daemon builds it. The manifest carries a
// bundle's manifest verbatim, as the build argument its label is set from, so
// seeding reads it from there: a harness bundle's own label from its
// HARNESS_METADATA, and the platform bundle's PlatformLayer from its
// IMAGE_LAYER_METADATA. A bundle inherits no layer from anything (ADR
// 26-10-16-423 §2), so nothing is walked.
//
// In copy-mode the watcher exports each bundle as an OCI image layout, which
// no daemon or registry holds either: layouts answers a bundle from there
// (inspectLayoutBundle).
type devImageInspector struct {
	bundles  map[string]map[string]string
	layouts  map[string]devimage.Bundle
	images   map[string]bool
	fallback imageInspector
	verified *verifiedBundles
}

// The build targets of the bundle stages, which say which label an entry's
// metadata argument becomes. Keep in sync with sandbox-agent/Dockerfile and
// the harness Dockerfiles.
const (
	platformBundleTarget = "platform"
	harnessBundleTarget  = "bundle"
)

// newDevImageInspector wraps fallback with the build-mode manifest entries in
// images and the exported bundles. It returns fallback unchanged when there
// are neither, so production keeps the original behavior exactly.
func newDevImageInspector(images []devimage.Image, bundles []devimage.Bundle, fallback imageInspector, verified *verifiedBundles) imageInspector {
	inspector := devImageInspector{
		bundles: map[string]map[string]string{}, layouts: map[string]devimage.Bundle{}, images: map[string]bool{},
		fallback: fallback, verified: verified,
	}
	for _, bundle := range bundles {
		inspector.layouts[bundle.Reference] = bundle
	}
	for _, image := range images {
		if image.Build == nil {
			continue
		}
		reference := strings.TrimSpace(image.Reference)
		switch strings.TrimSpace(image.Build.Target) {
		case platformBundleTarget:
			inspector.bundles[reference] = map[string]string{
				harness.ImageLayerLabelPrefix + harness.PlatformLayer: strings.TrimSpace(image.Build.Args[harness.LayerMetadataBuildArg]),
			}
		case harnessBundleTarget:
			inspector.bundles[reference] = map[string]string{
				harness.ImageLabel: strings.TrimSpace(image.Build.Args[harness.MetadataBuildArg]),
			}
		default:
			inspector.images[reference] = true
		}
	}
	if len(inspector.bundles) == 0 && len(inspector.layouts) == 0 && len(inspector.images) == 0 {
		return fallback
	}
	return inspector
}

// Inspect answers a build-mode reference with itself as its digest: it is
// content-addressed over that image's inputs, so it is its own freshness key,
// and there is no digest until it is built. Nor is there a platform: the pool
// that runs it builds it for its own, so the set is left empty and rules no
// pool out. InspectBundle answers a bundle's the same way.
func (d devImageInspector) Inspect(ctx context.Context, imageRef string) (imageMetadata, error) {
	if !d.images[strings.TrimSpace(imageRef)] {
		return d.fallback.Inspect(ctx, imageRef)
	}
	return runnableImage(imageRef), nil
}

func (d devImageInspector) InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error) {
	if bundle, ok := d.layouts[strings.TrimSpace(bundleRef)]; ok {
		return inspectLayoutBundle(bundle, admit, d.verified)
	}
	labels, ok := d.bundles[strings.TrimSpace(bundleRef)]
	if !ok {
		return d.fallback.InspectBundle(ctx, bundleRef, admit)
	}
	if err := admitBundle(labels, admit); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q: %w", bundleRef, err)
	}
	return bundleInspection{Digest: bundleRef, Labels: labels}, nil
}
