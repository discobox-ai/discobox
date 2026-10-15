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

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type imageMetadata struct {
	// Digest is a manifest file's, which a sandbox on its template pins, and
	// BundleDigest a bundle's, which a box on it applies (ADR 26-10-16-423 §2).
	// One of the two is set: a harness is a bundle or a manifest file.
	Digest       string
	BundleDigest string
	// Platforms is what a manifest file's template runs on (ADR 0145 §1): the
	// one platform it declares. Empty for a bundle, whose steps select the
	// platform they run on, which rules no pool out.
	Platforms platform.Set
	// ImageMetadata is the resolved manifest. Its ImageKind is the kind of box
	// the harness runs on (ADR 26-10-09-106 §4), always set once inspected: OCI
	// for a bundle, which a Docker pool's sandbox base image applies, or the
	// VM kind a manifest file declares.
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
// InspectBundle reads a bundle, refusing one carrying a whiteout: a bundle's
// layers add feature directories side by side, and remove nothing (ADR
// 26-10-16-423 §2). admit judges its labels before any layer is read, so a
// reference that is not the bundle it is meant to be — an image of several GB
// named as a harness's bundle — is refused for what its labels say, not after
// its layers are saved and read. InspectManifestFile reads a manifest file,
// whose layers are the whole manifest of a template with no image.
type imageInspector interface {
	InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error)
	InspectManifestFile(ctx context.Context, ref string) (imageMetadata, error)
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

// InspectManifestFile reads a template with no image — a non-Linux one,
// assembled from a vendor's base and Discobox's overlay — named by its
// overlay's manifest file (ADR 0145 §3), which is never a reference a daemon
// or a registry knows.
func (d defaultImageInspector) InspectManifestFile(_ context.Context, ref string) (imageMetadata, error) {
	if !isManifestFile(ref) {
		return imageMetadata{}, fmt.Errorf("%q names no manifest file", ref)
	}
	return inspectManifestFile(ref, d.overlayDir)
}

func (d defaultImageInspector) InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error) {
	if isManifestFile(bundleRef) {
		return bundleInspection{}, fmt.Errorf("bundle %q names a manifest file: a bundle is an OCI image", bundleRef)
	}
	// A locally present bundle is authoritative once found: its metadata is
	// what was built here, and an error reading it is surfaced rather than
	// masked by a doomed registry fetch of the same (often :local) reference.
	if local, found, err := inspectLocalBundle(ctx, bundleRef, admit, d.verified); found {
		return local, err
	}
	ref, err := name.ParseReference(bundleRef)
	if err != nil {
		return bundleInspection{}, fmt.Errorf("parse bundle %q: %w", bundleRef, err)
	}
	// One GET, which answers the digest and, for the platform bundle, its
	// index. Not remote.Head: ghcr.io answers HEAD without a Content-Length
	// header, which go-containerregistry rejects. The platform is always
	// named: go-containerregistry answers linux/amd64 when nothing says
	// otherwise.
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
// image built FROM discobox-sandbox-base carries the ReclaimLabel it sets: the
// base itself, the pool agent's, a snapshot, and the runnable images of a
// release from before bundles, which carried the manifest labels too. That is
// the one label to tell them apart by, so none of those several-GB images is
// admitted as a bundle and has its layers saved and read.
func admitBundle(labels map[string]string, admit func(labels map[string]string) error) error {
	if _, runnable := labels[harness.ReclaimLabel]; runnable {
		return fmt.Errorf("it carries %s, so it is an image built FROM discobox-sandbox-base, and a bundle is built FROM scratch", harness.ReclaimLabel)
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

// publishedImage resolves the image a bundle's labels and layers are read
// from, and, for a multi-platform one — the platform bundle — every platform
// its index publishes. They are read from the platform a pool on this machine
// hosts when the index lists it, and from the first platform it lists
// otherwise: a bundle published only for another architecture still declares
// the same manifest.
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

// localImageDigest is the digest to pin a locally-inspected bundle to.
//
// A pulled bundle carries the registry digest in RepoDigests, which is the one
// its registry serves it under. A locally built one has none — nothing pushed
// it — so its image ID is all there is: a containerd image store makes that ID
// the image's manifest digest; the classic store makes it its config's.
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

// isManifestFile reports whether ref names a manifest file rather than an
// image: services.ManifestFileScheme and an absolute path on this server's
// host, inside its overlay directory.
func isManifestFile(ref string) bool {
	return strings.HasPrefix(strings.TrimSpace(ref), services.ManifestFileScheme)
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
// a VM image for a driver (ADR 26-10-09-106 §4): an OCI template is an
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
	if metadata.ImageKind.Format != platform.FormatVM {
		return imageMetadata{}, fmt.Errorf("manifest file declares no VM image kind: a template with no image names the VM driver its image is built for, as %s/<driver>", platform.FormatVM)
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
// provider's (ADR 26-10-16-423 §2). The kind is OCI here, the kind of box a
// Docker pool runs it on: its sandbox base image, which the agent applies it
// to.
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

// devImageInspector answers a development bundle before falling back to a
// daemon or registry lookup, neither of which holds one.
//
// In copy-mode the watcher exports each bundle as an OCI image layout, and in
// build-mode a pool's daemon builds it and the server saves its layout
// (devbundles): either is read from there (inspectLayoutBundle). Until a pool
// has built it, a build-mode bundle exists only as its build description,
// which carries its manifest verbatim as the build argument its label is set
// from, so seeding reads it from there: a harness bundle's own label from its
// HARNESS_METADATA, and the platform bundle's PlatformLayer from its
// IMAGE_LAYER_METADATA. A bundle inherits no layer from anything (ADR
// 26-10-16-423 §2), so nothing is walked.
type devImageInspector struct {
	bundles  *devbundles.Set
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

// newDevImageInspector wraps fallback with the development bundles. It
// returns fallback unchanged when there are none, so production keeps the
// original behavior exactly.
func newDevImageInspector(bundles *devbundles.Set, fallback imageInspector, verified *verifiedBundles) imageInspector {
	if bundles == nil {
		return fallback
	}
	return devImageInspector{bundles: bundles, fallback: fallback, verified: verified}
}

func (d devImageInspector) InspectManifestFile(ctx context.Context, ref string) (imageMetadata, error) {
	return d.fallback.InspectManifestFile(ctx, ref)
}

// InspectBundle answers a bundle no pool has built yet with its reference as
// its digest: the reference is content-addressed over the build's inputs, so
// it is its own freshness key, and there is no digest until it is built. A
// box is never pinned to one (resources/sandboxes), and the built bundle's
// digest replaces it once its layout is saved and seeding runs again.
func (d devImageInspector) InspectBundle(ctx context.Context, bundleRef string, admit func(labels map[string]string) error) (bundleInspection, error) {
	bundleRef = strings.TrimSpace(bundleRef)
	if bundle, ok := d.bundles.Layout(bundleRef); ok {
		return inspectLayoutBundle(bundle, admit, d.verified)
	}
	bundle, ok := d.bundles.Unbuilt(bundleRef)
	if !ok {
		return d.fallback.InspectBundle(ctx, bundleRef, admit)
	}
	var labels map[string]string
	switch strings.TrimSpace(bundle.Build.Target) {
	case platformBundleTarget:
		labels = map[string]string{
			harness.ImageLayerLabelPrefix + harness.PlatformLayer: strings.TrimSpace(bundle.Build.Args[harness.LayerMetadataBuildArg]),
		}
	case harnessBundleTarget:
		labels = map[string]string{
			harness.ImageLabel: strings.TrimSpace(bundle.Build.Args[harness.MetadataBuildArg]),
		}
	default:
		return bundleInspection{}, fmt.Errorf("bundle %q builds the target %q, which is no bundle stage", bundleRef, bundle.Build.Target)
	}
	if err := admitBundle(labels, admit); err != nil {
		return bundleInspection{}, fmt.Errorf("bundle %q: %w", bundleRef, err)
	}
	return bundleInspection{Digest: bundleRef, Labels: labels}, nil
}
