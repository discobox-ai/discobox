// Package guestimage resolves the boot artifacts a VM driver needs — a kernel,
// an initrd, a root filesystem — from an OCI image, on a host that has no
// Docker daemon.
//
// This is what lets a pool backend bootstrap itself on macOS (ADR 0062 §5).
// The guest image is built and released on its own line, and it is fetched
// through the image store the server hands its providers (imagecache, ADR 0113
// §5): no daemon, no crane binary, and nothing to install before the first pool
// starts. A guest the CLI staged before starting the server is extracted from
// local blobs; one it did not is downloaded into the store on the way.
//
// Resolution has two modes, and a driver treats them identically:
//
//   - A reference is fetched once per digest and its artifacts extracted under
//     CacheDir. That cache is content-addressed by the image's manifest digest,
//     so a new guest release lands beside the old one rather than replacing it
//     in place, and an interrupted extraction can never be mistaken for a
//     complete one.
//   - An override directory is used as-is. That is how a guest image built from
//     local sources — including one built inside a running pool VM and exported
//     back to the host (ADR 0062 §7) — is booted without publishing it.
package guestimage

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// Artifact is one file a driver needs out of the guest image.
type Artifact struct {
	// Name is the file's path inside the image, without a leading slash.
	Name string
	// Optional keeps resolution succeeding when the image does not carry this
	// file. A driver that boots without an initrd declares it optional rather
	// than branching on a missing-file error later.
	Optional bool
}

// Config describes one driver's guest artifact set.
type Config struct {
	// Reference is the guest image. A digest-pinned reference is the intended
	// form: it makes a server build boot a known guest and decouples the guest's
	// release line from the server's (ADR 0062 §3). A tag is accepted for
	// development and still caches by the digest it resolves to.
	Reference string
	// Artifacts are the files to extract. Resolution fails when a required one
	// is absent, naming the image, so a malformed guest release is reported
	// where it can be acted on rather than as a boot failure inside a VM.
	Artifacts []Artifact
	// CacheDir holds one directory per resolved digest.
	CacheDir string
	// OverrideDir, when set, is used directly and nothing is pulled. It is an
	// assertion: an incomplete override is an error, not a reason to fall back,
	// because someone said explicitly which artifacts to boot.
	OverrideDir string
	// LocalDir is preferred over Reference when it holds a complete artifact
	// set, and ignored when it does not. It is the conventional output of a
	// local guest image build, so building one is the whole act of adopting it —
	// there is nothing to configure, and removing the directory goes back to the
	// published image.
	LocalDir string
	// Platform is the image platform to select, defaulting to linux on the
	// host's architecture. A VM guest is Linux whatever the host runs, and the
	// architecture must match the hypervisor's, which is the host's.
	Platform *v1.Platform
	// Images is the image store a reference is fetched through (ADR 0113 §5).
	// A resolver only checking its configuration may have none; resolving a
	// reference without one is an error, and an override directory needs none.
	Images *imagecache.Layout
	// Client fetches from the registry when the store lacks something. Nil is
	// the store's own.
	Client *http.Client
}

// Bundle is a resolved artifact set on local disk.
type Bundle struct {
	// Dir is the directory holding the artifacts.
	Dir string
	// Source describes where the bundle came from, for logs and for the pool
	// runtime's own record of what it booted: a digest, or "override".
	Source string

	paths map[string]string
}

// Path returns the absolute path of one artifact, or "" when the image did not
// carry it and it was declared optional.
func (b *Bundle) Path(artifact string) string {
	if b == nil {
		return ""
	}
	return b.paths[artifact]
}

// Progress is one report about a guest image being fetched, shaped for a status
// line rather than for a progress bar.
//
// Current counts the compressed bytes read from the registry into the image
// store, which is what the wait is actually made of. Total is the sum of the
// sizes of what the store lacked, declared by the manifest, so unlike a Docker
// pull it is known before the first byte and does not grow. An image already in
// the store reports nothing: extracting it is local work with no denominator.
type Progress struct {
	// Reference is the image being fetched.
	Reference string
	// Current and Total are compressed bytes.
	Current int64
	Total   int64
	// Layers and LayersComplete count the blobs the fetch is made of: how many
	// the store had to download, and how many are in. Both are zero for an
	// image it already held, which downloads nothing.
	Layers         int
	LayersComplete int
	// Done marks the closing report, sent once the artifacts are on disk.
	Done bool
}

// ProgressFunc receives fetch progress. A nil one asks for none, and then
// nothing is wrapped and no reporting goroutine runs.
//
// It is called from the extraction's own goroutine and may be called often;
// an implementation that blocks slows the fetch down.
type ProgressFunc func(Progress)

// progressInterval is how often a fetch in flight is reported. It matches the
// pool image pull's rate, because both end up on the same status line and a
// byte counter has to move at about this rate to read as movement. The store
// reports on a ticker rather than on byte arrival, so a stalled download keeps
// restating the phase.
const progressInterval = 500 * time.Millisecond

// Resolver resolves one driver's guest artifacts, at most once per process for
// a given digest. It is safe for concurrent use: pools start in parallel and
// would otherwise each pull the same image.
type Resolver struct {
	cfg Config

	mu       sync.Mutex
	resolved *Bundle
}

// New validates a guest image configuration without touching the network.
func New(cfg Config) (*Resolver, error) {
	if len(cfg.Artifacts) == 0 {
		return nil, errors.New("guestimage: at least one artifact is required")
	}
	seen := map[string]struct{}{}
	for _, artifact := range cfg.Artifacts {
		clean, err := cleanArtifactName(artifact.Name)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[clean]; ok {
			return nil, fmt.Errorf("guestimage: duplicate artifact %q", clean)
		}
		seen[clean] = struct{}{}
	}
	if local := strings.TrimSpace(cfg.LocalDir); local != "" {
		if !filepath.IsAbs(local) {
			return nil, fmt.Errorf("guestimage: local directory %q must be an absolute path", local)
		}
		cfg.LocalDir = filepath.Clean(local)
	}
	if override := strings.TrimSpace(cfg.OverrideDir); override != "" {
		if !filepath.IsAbs(override) {
			return nil, fmt.Errorf("guestimage: override directory %q must be an absolute path", override)
		}
		cfg.OverrideDir = filepath.Clean(override)
		return &Resolver{cfg: cfg}, nil
	}
	if strings.TrimSpace(cfg.Reference) == "" {
		return nil, errors.New("guestimage: a reference or an override directory is required")
	}
	if _, err := name.ParseReference(strings.TrimSpace(cfg.Reference)); err != nil {
		return nil, fmt.Errorf("guestimage: parse reference %q: %w", cfg.Reference, err)
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		return nil, errors.New("guestimage: a cache directory is required")
	}
	if !filepath.IsAbs(cfg.CacheDir) {
		return nil, fmt.Errorf("guestimage: cache directory %q must be an absolute path", cfg.CacheDir)
	}
	cfg.CacheDir = filepath.Clean(cfg.CacheDir)
	cfg.Reference = strings.TrimSpace(cfg.Reference)
	return &Resolver{cfg: cfg}, nil
}

// LocalDir is where a local guest image build is looked for, or "" when this
// resolver has none — an override directory names the artifacts outright, so
// there is nothing for a build to be adopted into.
//
// A caller that builds a guest image writes it here, which is the whole of
// adopting one: resolution prefers a complete local build over the published
// image, and deleting the directory goes back.
func (r *Resolver) LocalDir() string {
	if r.cfg.OverrideDir != "" {
		return ""
	}
	return r.cfg.LocalDir
}

// Invalidate drops the memoized bundle, so the next Resolve looks at the disk
// again.
//
// Resolution is memoized for the life of the process, which is right for a
// value that cannot change under a running server — except when this server is
// what changed it. A build that publishes a new local guest calls this, or the
// server keeps booting the guest it resolved first for as long as it runs.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = nil
}

// Reference reports the configured guest image reference, or "" when an
// override directory is in use. A LocalDir does not clear it: whether the local
// build is usable is only known once it is inspected.
func (r *Resolver) Reference() string {
	if r.cfg.OverrideDir != "" {
		return ""
	}
	return r.cfg.Reference
}

// Resolve returns the artifact bundle, pulling and extracting the guest image
// the first time it is needed on this host.
//
// report, when set, narrates the pull. It is only ever called for a fetch that
// actually moves bytes: an override directory, a local build, and a cache hit
// all resolve without touching the network and have nothing to narrate.
func (r *Resolver) Resolve(ctx context.Context, report ProgressFunc) (*Bundle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolved != nil {
		return r.resolved, nil
	}
	bundle, err := r.resolve(ctx, report)
	if err != nil {
		return nil, err
	}
	r.resolved = bundle
	return bundle, nil
}

func (r *Resolver) resolve(ctx context.Context, report ProgressFunc) (*Bundle, error) {
	if r.cfg.OverrideDir != "" {
		bundle, err := r.collect(r.cfg.OverrideDir, "override")
		if err != nil {
			return nil, fmt.Errorf("guestimage: configured guest artifacts in %s: %w", r.cfg.OverrideDir, err)
		}
		slog.InfoContext(ctx, "using configured guest image artifacts", "dir", r.cfg.OverrideDir)
		return bundle, nil
	}
	// A locally built guest image wins over the published one, and an absent or
	// half-built one is simply not there. Reporting why it was skipped matters:
	// otherwise a developer who built a guest and is quietly running the
	// published image has no way to tell.
	if r.cfg.LocalDir != "" {
		bundle, err := r.collect(r.cfg.LocalDir, "local")
		if err == nil {
			slog.InfoContext(ctx, "using locally built guest image artifacts", "dir", r.cfg.LocalDir)
			return bundle, nil
		}
		slog.DebugContext(ctx, "no locally built guest image artifacts",
			"dir", r.cfg.LocalDir, "reason", err)
	}

	ref, err := name.ParseReference(r.cfg.Reference)
	if err != nil {
		return nil, fmt.Errorf("guestimage: parse reference %q: %w", r.cfg.Reference, err)
	}
	pinned, isPinned := ref.(name.Digest)
	if isPinned {
		// A complete cache directory is the whole freshness check, and for a
		// pinned reference it is known without asking anything: extraction
		// renames a fully written directory into place, so one that exists
		// with every required artifact in it was written by a completed
		// extraction of exactly this digest.
		if hash, err := v1.NewHash(pinned.DigestStr()); err == nil {
			if bundle, err := r.collect(r.digestDir(hash), hash.String()); err == nil {
				return bundle, nil
			}
		}
	} else {
		// Not an error: a tag is how a developer points at a guest build that has
		// no digest yet. It is worth saying out loud, because it is the one way
		// two servers on the same tag can boot different guests.
		slog.WarnContext(ctx, "guest image reference is not digest-pinned", "reference", r.cfg.Reference)
	}
	if r.cfg.Images == nil {
		return nil, fmt.Errorf("guestimage: no image store to fetch %s through", r.cfg.Reference)
	}

	platform := r.platform()
	fetched, err := r.cfg.Images.Fetch(ctx, r.cfg.Reference, imagecache.Options{
		Platform:    imagecache.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant},
		Client:      r.cfg.Client,
		Credentials: registryauth.Credentials,
		// A tag is asked about on every resolution, as it always was: it is
		// how a developer's pushed guest reaches a running machine.
		Revalidate: !isPinned,
		OnProgress: fetchProgress(r.cfg.Reference, report),
	})
	if err != nil {
		return nil, fmt.Errorf("guestimage: fetch %s: %w", r.cfg.Reference, registryauth.Explain(ref, err))
	}
	digest := fetched.Digest()
	hash, err := v1.NewHash(digest)
	if err != nil {
		return nil, fmt.Errorf("guestimage: %s: %w", r.cfg.Reference, err)
	}
	dir := r.digestDir(hash)
	if bundle, err := r.collect(dir, digest); err == nil {
		return bundle, nil
	}

	image, err := storedImage(fetched)
	if err != nil {
		return nil, fmt.Errorf("guestimage: read %s: %w", r.cfg.Reference, err)
	}
	if err := r.checkPlatform(image); err != nil {
		return nil, err
	}
	if err := r.extract(ctx, image, dir); err != nil {
		return nil, err
	}
	bundle, err := r.collect(dir, digest)
	if err != nil {
		return nil, fmt.Errorf("guestimage: %s does not carry the expected artifacts: %w", r.cfg.Reference, err)
	}
	slog.InfoContext(ctx, "extracted guest image artifacts",
		"reference", r.cfg.Reference, "digest", digest, "dir", dir)
	return bundle, nil
}

// fetchProgress narrates a fetch through the store, at the rate the pool image
// pull reports at — both land on the same status line. The store reports only a
// fetch that goes to the registry, so a guest already in it resolves without a
// word, as a cache hit always did.
func fetchProgress(reference string, report ProgressFunc) func(imagecache.Progress) {
	if report == nil {
		return nil
	}
	var last time.Time
	return func(fetch imagecache.Progress) {
		// The store opens with a report that names the image before its
		// manifests say how big it is. The driver has announced the phase
		// already, and a byte counter with no denominator says nothing more.
		if fetch.Total == 0 && !fetch.Done {
			return
		}
		if !fetch.Done && time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		report(Progress{
			Reference:      reference,
			Current:        fetch.Current,
			Total:          fetch.Total,
			Layers:         fetch.Layers,
			LayersComplete: fetch.LayersComplete,
			Done:           fetch.Done,
		})
	}
}

// checkPlatform refuses an image built for another architecture.
//
// Selecting a child of an index does nothing at all to a plain
// single-architecture manifest, which is returned whatever was asked for. That
// is a silent failure with no good symptom: the artifacts extract, the VM
// boots, and the guest panics on its first instruction. The image's own config
// is the authority, so it is what gets checked — here as well as in the store,
// because this is the check whose failure names the guest.
//
// Only a stated mismatch is refused. An image that declares no architecture
// makes no claim to contradict, and refusing it would reject a hand-assembled
// artifact set that boots perfectly well.
func (r *Resolver) checkPlatform(image v1.Image) error {
	config, err := image.ConfigFile()
	if err != nil {
		return fmt.Errorf("guestimage: read %s configuration: %w", r.cfg.Reference, err)
	}
	want := r.platform()
	if (config.Architecture == "" || config.Architecture == want.Architecture) &&
		(config.OS == "" || config.OS == want.OS) {
		return nil
	}
	return fmt.Errorf("guestimage: %s is built for %s/%s, but this host needs %s/%s",
		r.cfg.Reference, config.OS, config.Architecture, want.OS, want.Architecture)
}

// extract flattens the image and writes the wanted artifacts into dir. It
// builds a temporary directory and renames it, so a partially extracted
// directory is never visible under a digest.
func (r *Resolver) extract(ctx context.Context, image v1.Image, dir string) error {
	if err := os.MkdirAll(r.cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("guestimage: create cache directory: %w", err)
	}
	staging, err := os.MkdirTemp(r.cfg.CacheDir, ".extract-")
	if err != nil {
		return fmt.Errorf("guestimage: create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// Optional artifacts are extracted when present but never demanded: a driver
	// that boots without an initrd should not fail on a guest image that omits
	// one.
	wanted := map[string]struct{}{}
	required := map[string]struct{}{}
	for _, artifact := range r.cfg.Artifacts {
		clean, err := cleanArtifactName(artifact.Name)
		if err != nil {
			return err
		}
		wanted[clean] = struct{}{}
		if !artifact.Optional {
			required[clean] = struct{}{}
		}
	}

	contents := mutate.Extract(image)
	defer func() { _ = contents.Close() }()
	reader := tar.NewReader(contents)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("guestimage: read %s: %w", r.cfg.Reference, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		entry := strings.TrimPrefix(path.Clean("/"+header.Name), "/")
		if _, ok := wanted[entry]; !ok {
			continue
		}
		// The destination is derived from the wanted set, never from the tar
		// header, so a crafted image cannot write outside the staging directory.
		destination := filepath.Join(staging, filepath.FromSlash(entry))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("guestimage: create %s: %w", filepath.Dir(destination), err)
		}
		if err := writeFile(destination, reader, header.FileInfo().Mode().Perm()); err != nil {
			return err
		}
		delete(wanted, entry)
		delete(required, entry)
	}
	if len(required) > 0 {
		return fmt.Errorf("guestimage: %s is missing %s", r.cfg.Reference, strings.Join(sortedKeys(required), ", "))
	}

	if err := os.Rename(staging, dir); err != nil {
		// Losing the rename race means another resolution completed first, and
		// its directory is as good as this one: both came from the same digest.
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return fmt.Errorf("guestimage: publish %s: %w", dir, err)
	}
	return nil
}

// collect reports the bundle in dir, failing when a required artifact is
// missing or empty.
func (r *Resolver) collect(dir, source string) (*Bundle, error) {
	paths := make(map[string]string, len(r.cfg.Artifacts))
	for _, artifact := range r.cfg.Artifacts {
		clean, err := cleanArtifactName(artifact.Name)
		if err != nil {
			return nil, err
		}
		full := filepath.Join(dir, filepath.FromSlash(clean))
		info, err := os.Stat(full)
		switch {
		case err == nil && info.Mode().IsRegular() && info.Size() > 0:
			paths[artifact.Name] = full
		case artifact.Optional:
			continue
		case err != nil:
			return nil, fmt.Errorf("artifact %s: %w", clean, err)
		default:
			return nil, fmt.Errorf("artifact %s is empty or not a regular file", clean)
		}
	}
	return &Bundle{Dir: dir, Source: source, paths: paths}, nil
}

func (r *Resolver) digestDir(digest v1.Hash) string {
	return filepath.Join(r.cfg.CacheDir, digest.Algorithm+"-"+digest.Hex)
}

func (r *Resolver) platform() v1.Platform {
	if r.cfg.Platform != nil {
		return *r.cfg.Platform
	}
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

func writeFile(destination string, contents io.Reader, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("guestimage: create %s: %w", destination, err)
	}
	if _, err := io.Copy(file, contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("guestimage: write %s: %w", destination, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("guestimage: write %s: %w", destination, err)
	}
	return nil
}

func cleanArtifactName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("guestimage: artifact name must not be empty")
	}
	clean := strings.TrimPrefix(path.Clean("/"+trimmed), "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("guestimage: invalid artifact name %q", raw)
	}
	return clean, nil
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
