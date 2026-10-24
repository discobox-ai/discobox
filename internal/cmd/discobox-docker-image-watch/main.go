package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
)

const envFile = ".env"
const developmentImageManifestFile = ".tmp/discobox-dev-images.json"

var bundleBuilder = flag.String("bundle-builder", "discobox", "Buildx builder for local OCI bundle exports when the host daemon's own cannot export them")

// bundleLayoutDir is where the watcher exports each bundle, as the OCI image
// layout tar <name>/<hex>.tar named by the digest of the manifest it holds,
// for the server to serve from (ADR 26-10-16-423 §13).
const bundleLayoutDir = "build/assets"

// baseSpecName is the Docker driver's sandbox base image, every sandbox
// container's, and the one runnable image the watcher builds.
const baseSpecName = "sandbox-base"

// platformSpecName is the platform bundle: the sandbox agent Dockerfile's
// FROM scratch stage holding every platform feature (ADR 26-10-16-423 §7).
const platformSpecName = "platform"

// agentSpecName is the agent bundle: the sandbox agent Dockerfile's FROM
// scratch stage holding the agent alone (ADR 26-10-24-953 §2).
const agentSpecName = "agent"

// agentBundleEnvKey is the .env key the server reads the agent bundle from.
// Keep in sync with the server's DISCOBOX_AGENT_BUNDLE setting.
const agentBundleEnvKey = "DISCOBOX_AGENT_BUNDLE"

// poolAgentBundleSpecName is the pool-agent bundle: pool-agent/Dockerfile's
// FROM scratch stage holding the agent and its units (ADR 26-10-23-551 §2).
const poolAgentBundleSpecName = "pool-agent-bundle"

// poolAgentBundleEnvKey is the .env key the server reads the pool-agent
// bundle from (its poolAgentBundle setting).
const poolAgentBundleEnvKey = "DISCOBOX_POOL_AGENT_BUNDLE"

// platformBundleEnvKey is the .env key the server reads the platform bundle
// from. Keep in sync with the server's DISCOBOX_PLATFORM_BUNDLE setting.
const platformBundleEnvKey = "DISCOBOX_PLATFORM_BUNDLE"

type imageSpec struct {
	name        string
	baseImage   string
	devPrefix   string
	envImageKey string
	buildDir    string
	buildArgs   []string
	files       []string
	// metadataFile is the authoring-time image.json compacted into metadataArg
	// at build time, which the Dockerfile turns into a manifest label. Both are
	// empty for an image whose build labels no manifest.
	metadataFile string
	metadataArg  string
	// contextDir, dockerfile and target describe the same build as buildArgs,
	// but declaratively, for build-mode: the server builds these on the
	// destination Docker daemon, so there is no docker CLI invocation to derive
	// them from. target is the stage, for a Dockerfile whose artifact is not
	// its last stage's or is named: the platform bundle, a harness's bundle.
	contextDir string
	dockerfile string
	target     string
	// bundle marks a FROM scratch bundle (ADR 26-10-16-423 §2): never run, so
	// copy-mode exports it into layoutDir, as an OCI image layout tar named by
	// its digest, rather than into any Docker daemon, and records it among the
	// manifest's bundles, which the server serves and reads its labels from.
	// Build-mode records its build among the bundles instead: a pool's daemon
	// builds it and the server saves its layout (server/internal/devbundles).
	bundle    bool
	layoutDir string
}

type harnessImage struct {
	name string
	dir  string
}

var harnessImages = []harnessImage{
	{name: "codex", dir: "codex-cli"},
	{name: "claude-code", dir: "claude-code"},
	{name: "opencode", dir: "opencode"},
	{name: "copilot", dir: "copilot"},
	{name: "omp", dir: "omp"},
	{name: "pi", dir: "pi"},
	{name: "shell", dir: "shell"},
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	ctx := context.Background()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return err
	}
	// Only local builds need host Docker. macOS and Windows default to
	// describing builds for the pool, and Linux can opt into that mode too.
	builder := ""
	if !buildModeEnabled() {
		if builder, err = localBundleBuilder(ctx, repoRoot); err != nil {
			return err
		}
	}
	specs, err := dockerImageSpecs(ctx, repoRoot, builder)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return errors.New("no Docker images configured")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// A separate, slower beat for "is the image still there", which costs a
	// Docker call where the file check costs a stat.
	presence := time.NewTicker(missingImageCheckInterval)
	defer presence.Stop()
	// And a third, for which files are inputs at all. The file check stats a
	// fixed list; a Go file added to a package, or a package newly imported,
	// is not on it until the list is discovered again.
	discovery := time.NewTicker(inputDiscoveryInterval)
	defer discovery.Stop()
	rediscover := func(ctx context.Context) ([]imageSpec, error) { return dockerImageSpecs(ctx, repoRoot, builder) }
	return watchImages(ctx, repoRoot, specs, ticker.C, presence.C, discovery.C, rediscover)
}

// watchImages rebuilds images whose inputs change. rediscover, called on each
// discovery tick, lists the inputs again: an image whose set of input files
// changed is rebuilt just as one whose files did.
func watchImages(ctx context.Context, repoRoot string, specs []imageSpec, ticks, presence, discovery <-chan time.Time, rediscover func(context.Context) ([]imageSpec, error)) error {
	states := make(map[string]map[string]fileState, len(specs))
	pending := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if len(spec.files) == 0 {
			return fmt.Errorf("no Docker inputs discovered for %s", spec.name)
		}
		log.Printf("watching %d Docker inputs for %s", len(spec.files), spec.name)
		states[spec.name] = snapshot(spec.files)
		pending[spec.name] = true
	}
	var retryAt time.Time
	now := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(pending) > 0 && !now.Before(retryAt) {
			var builds []imageSpec
			for _, spec := range specs {
				if pending[spec.name] {
					builds = append(builds, spec)
				}
			}
			log.Printf("building development images: %s", strings.Join(specNames(builds), ", "))
			if err := buildChangedImages(ctx, repoRoot, specs, builds); err != nil {
				// Remember the whole pass until builds AND publication succeed.
				// An old :local tag can still exist after a failed build, so the
				// missing-image check cannot substitute for retrying pending work.
				retryAt = time.Now().Add(imageBuildRetryInterval)
				log.Printf("build failed; retrying in %s: %v", imageBuildRetryInterval, err)
			} else {
				clear(pending)
				retryAt = time.Time{}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now = <-discovery:
			fresh, err := rediscover(ctx)
			if err != nil {
				log.Printf("discover Docker inputs: %v", err)
				continue
			}
			for i := range specs {
				next, ok := specNamed(fresh, specs[i].name)
				if !ok || slices.Equal(next.files, specs[i].files) {
					continue
				}
				log.Printf("Docker inputs for %s changed: now %d files", specs[i].name, len(next.files))
				// The whole spec, not only its files: the metadata file and
				// build args are derived from the same inputs, and a stale one
				// names a file that is gone or misses one that was added.
				specs[i] = next
				states[specs[i].name] = snapshot(next.files)
				pending[specs[i].name] = true
			}
		case now = <-presence:
			missing, err := missingImageSpecs(ctx, repoRoot, specs)
			if err != nil {
				log.Printf("check built images: %v", err)
				continue
			}
			for _, spec := range missing {
				pending[spec.name] = true
			}
		case now = <-ticks:
			for _, spec := range specs {
				next := snapshot(spec.files)
				if changed(states[spec.name], next) {
					states[spec.name] = next
					pending[spec.name] = true
				}
			}
		}
	}
}

// inputDiscoveryInterval paces listing each image's inputs again, which costs a
// `go list` per Go image where the file check costs a stat per file.
const inputDiscoveryInterval = 10 * time.Second

func specNamed(specs []imageSpec, name string) (imageSpec, bool) {
	for _, spec := range specs {
		if spec.name == name {
			return spec, true
		}
	}
	return imageSpec{}, false
}

// imageBuildRetryInterval bounds retries while continuing to collect file changes.
const imageBuildRetryInterval = 15 * time.Second

// missingImageCheckInterval paces the check for built images that have left the
// daemon.
const missingImageCheckInterval = 15 * time.Second

// missingImageSpecs returns the specs whose built image is no longer on the
// daemon.
//
// Rebuilding on a file change alone is not enough, because a built image can
// leave without any file changing: image reclamation removes a superseded one
// (ADR 0040), and a developer's own `docker system prune` removes all of them.
// Nothing then rebuilds it, while `.env` and the manifest keep naming it, so
// every pool reconcile fails against an image that cannot come back. This is the
// level-triggered half the watcher was missing: what it published must still
// exist, not merely have existed once.
func missingImageSpecs(ctx context.Context, repoRoot string, specs []imageSpec) ([]imageSpec, error) {
	if buildModeEnabled() {
		// Nothing is built on this host, so there is no local image to miss.
		return nil, nil
	}
	listed, err := commandOutput(ctx, repoRoot, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	tags := map[string]struct{}{}
	for _, tag := range strings.Fields(listed) {
		tags[tag] = struct{}{}
	}
	return missingFrom(specs, builtIn(tags, publishedBundles(repoRoot))), nil
}

// builtIn reports whether a spec's build is still where it was published: an
// image among the daemon's tags, or a bundle's layout among published, since a
// bundle is in no daemon.
func builtIn(tags map[string]struct{}, published []devimage.Bundle) func(imageSpec) bool {
	return func(spec imageSpec) bool {
		if spec.bundle {
			_, ok := publishedBundle(spec, published)
			return ok
		}
		_, ok := tags[spec.baseImage]
		return ok
	}
}

// publishedBundles is the bundles the development manifest last published,
// none when there is no manifest to read.
func publishedBundles(repoRoot string) []devimage.Bundle {
	manifest, err := devimage.Read(filepath.Join(repoRoot, developmentImageManifestFile))
	if err != nil {
		return nil
	}
	return manifest.Bundles
}

// publishedBundle is spec's bundle among published, while its layout is still
// there to serve it from.
func publishedBundle(spec imageSpec, published []devimage.Bundle) (devimage.Bundle, bool) {
	for _, bundle := range published {
		if !strings.HasPrefix(bundle.Reference, spec.devPrefix) || filepath.Dir(bundle.Layout) != spec.layoutDir {
			continue
		}
		if _, err := os.Stat(bundle.Layout); err == nil {
			return bundle, true
		}
	}
	return devimage.Bundle{}, false
}

// missingFrom is the decision behind missingImageSpecs, over whether each
// spec's built image or bundle layout is present.
func missingFrom(specs []imageSpec, present func(imageSpec) bool) []imageSpec {
	var out []imageSpec
	for _, spec := range specs {
		if !present(spec) {
			out = append(out, spec)
		}
	}
	return out
}

func containsSpec(specs []imageSpec, name string) bool {
	for _, spec := range specs {
		if spec.name == name {
			return true
		}
	}
	return false
}

func specNames(specs []imageSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.name)
	}
	return names
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "Taskfile.yml")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "pool-agent", "go.mod")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find repository root")
		}
	}
}

// localBundleBuilder is the buildx builder local OCI bundle exports use: the
// host daemon's own when it keeps images in containerd's store, which is what
// lets its builder export an OCI layout; otherwise the docker-container
// builder -bundle-builder names, prepared here. The host's own is preferred
// because it runs a build's steps through the daemon's runtime, which inside
// a discobox is what carries the proxy and its CA into them (ADR 0020); a
// docker-container builder runs them with its own. It is named, never left to
// buildx, which would take whichever builder is selected (BUILDX_BUILDER, the
// last `docker buildx use`): the daemon's own is the builder named after its
// Docker context.
func localBundleBuilder(ctx context.Context, repoRoot string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .DriverStatus}}").Output()
	if err != nil {
		return "", fmt.Errorf("read the host daemon's image store: %w", err)
	}
	if containerdImageStore(out) {
		context, err := exec.CommandContext(ctx, "docker", "context", "show").Output()
		if err != nil {
			return "", fmt.Errorf("read the host daemon's Docker context: %w", err)
		}
		return strings.TrimSpace(string(context)), nil
	}
	if err := runCommand(ctx, repoRoot, "go", "tool", "task", "buildx:builder", "BUILDX_BUILDER="+*bundleBuilder); err != nil {
		return "", fmt.Errorf("prepare bundle builder: %w", err)
	}
	return *bundleBuilder, nil
}

// containerdImageStore reports whether `docker info`'s DriverStatus, as JSON,
// is the containerd image store's.
func containerdImageStore(driverStatus []byte) bool {
	var status [][]string
	if err := json.Unmarshal(driverStatus, &status); err != nil {
		return false
	}
	for _, pair := range status {
		if len(pair) == 2 && pair[0] == "driver-type" && pair[1] == "io.containerd.snapshotter.v1" {
			return true
		}
	}
	return false
}

// dockerImageSpecs lists every image the watcher builds. builder is the
// buildx builder bundles are exported with (localBundleBuilder); empty, in
// build mode, where nothing is built here, names none.
func dockerImageSpecs(ctx context.Context, repoRoot, builder string) ([]imageSpec, error) {
	// The sandbox base image every sandbox container and every Docker pool's
	// runs (ADR 26-10-24-953 §2). Every bundle is FROM scratch, so none of
	// them is built on it.
	baseDir := filepath.Join("server", "providers", "dockerworker", "sandbox-image")
	baseSeen := map[string]struct{}{}
	if err := addTree(filepath.Join(repoRoot, baseDir), baseSeen); err != nil {
		return nil, err
	}
	workerRoot := filepath.Join(repoRoot, "pool-agent")
	workerFiles, err := goModuleFiles(ctx, workerRoot, repoRoot, "./cmd/discobox-pool-agent")
	if err != nil {
		return nil, err
	}
	workerSeen := make(map[string]struct{}, len(workerFiles)+8)
	for _, file := range workerFiles {
		workerSeen[file] = struct{}{}
	}
	for _, rel := range []string{
		"Dockerfile",
		"go.mod",
		"go.sum",
		"../.dockerignore",
		"../go.mod",
		"../go.sum",
		"../id/id.go",
	} {
		addFile(workerRoot, rel, workerSeen)
	}
	// The pool bundle is FROM scratch: the agent's build and the features it
	// carries, and not the shared base.
	poolAgentBundleSeen := workerSeen
	if err := addTree(filepath.Join(workerRoot, "bundle", "features"), poolAgentBundleSeen); err != nil {
		return nil, err
	}
	sandboxRoot := filepath.Join(repoRoot, "sandbox-agent")
	sandboxSeen := map[string]struct{}{}
	// The whole tree, so non-Go assets (image scripts, unit files) count too.
	if err := addTree(sandboxRoot, sandboxSeen); err != nil {
		return nil, err
	}
	// The binary the Dockerfile builds, so every root-module package it copies
	// into the build context is watched without naming any of them.
	sandboxFiles, err := goModuleFiles(ctx, sandboxRoot, repoRoot, "./cmd/discobox-sandbox-agent")
	if err != nil {
		return nil, err
	}
	// The agent bundle is the agent's build alone: its Go inputs and the
	// files the build stage copies, none of the features.
	agentSeen := map[string]struct{}{}
	for _, file := range sandboxFiles {
		sandboxSeen[file] = struct{}{}
		agentSeen[file] = struct{}{}
	}
	for _, rel := range []string{".dockerignore", "go.mod", "go.sum", filepath.Join("sandbox-agent", "Dockerfile")} {
		addFile(repoRoot, rel, agentSeen)
	}
	for _, rel := range []string{
		".dockerignore",
		"go.mod",
		"go.sum",
	} {
		path := filepath.Join(repoRoot, rel)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if err := addTree(path, sandboxSeen); err != nil {
				return nil, err
			}
			continue
		}
		addFile(repoRoot, rel, sandboxSeen)
	}
	addFile(repoRoot, ".dockerignore", sandboxSeen)
	// The files the Dockerfile copies into the platform's features from
	// outside sandbox-agent: the brand assets, the prompt helper, and the
	// discobox-access skill.
	if err := addTree(filepath.Join(repoRoot, "assets", "brand"), sandboxSeen); err != nil {
		return nil, err
	}
	if err := addTree(filepath.Join(repoRoot, "access", "skills", "discobox-access"), sandboxSeen); err != nil {
		return nil, err
	}
	addFile(repoRoot, filepath.Join("harness", "prompt-answer.sh"), sandboxSeen)
	// The platform bundle is FROM scratch, so the shared base is not among
	// its inputs.
	platformSeen := sandboxSeen
	specs := []imageSpec{
		{
			name:        baseSpecName,
			baseImage:   "discobox-sandbox-base:local",
			devPrefix:   "discobox-sandbox-base:dev-",
			envImageKey: "DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE",
			buildDir:    repoRoot,
			buildArgs:   []string{"build", "-f", filepath.Join(baseDir, "Dockerfile"), "-t", "discobox-sandbox-base:local", baseDir},
			contextDir:  filepath.Join(repoRoot, baseDir),
			dockerfile:  "Dockerfile",
			files:       sortedFiles(baseSeen),
		},
		{
			// The pool bundle, a pool's box's one bundle (ADR 26-10-24-953
			// §2).
			name:        poolAgentBundleSpecName,
			devPrefix:   "discobox-pool-agent-bundle:dev-",
			envImageKey: poolAgentBundleEnvKey,
			buildDir:    repoRoot,
			buildArgs: bundleBuildArgs(builder, bundleLayoutDirOf(repoRoot, "discobox-pool-agent-bundle"),
				"-f", "pool-agent/Dockerfile", "--target", "bundle", "."),
			contextDir: repoRoot,
			dockerfile: "pool-agent/Dockerfile",
			target:     "bundle",
			bundle:     true,
			layoutDir:  bundleLayoutDirOf(repoRoot, "discobox-pool-agent-bundle"),
			files:      sortedFiles(poolAgentBundleSeen),
		},
		{
			name:        agentSpecName,
			devPrefix:   "discobox-agent:dev-",
			envImageKey: agentBundleEnvKey,
			buildDir:    repoRoot,
			buildArgs:   bundleBuildArgs(builder, bundleLayoutDirOf(repoRoot, "discobox-agent"), "-f", "sandbox-agent/Dockerfile", "--target", "agent", "."),
			contextDir:  repoRoot,
			dockerfile:  "sandbox-agent/Dockerfile",
			target:      "agent",
			bundle:      true,
			layoutDir:   bundleLayoutDirOf(repoRoot, "discobox-agent"),
			files:       sortedFiles(agentSeen),
		},
		{
			name:        platformSpecName,
			devPrefix:   "discobox-platform:dev-",
			envImageKey: platformBundleEnvKey,
			buildDir:    repoRoot,
			buildArgs: bundleBuildArgs(builder, bundleLayoutDirOf(repoRoot, "discobox-platform"), "-f", "sandbox-agent/Dockerfile", "--target", "platform",
				"--build-arg", harness.LayerMetadataBuildArg+"=", "."),
			contextDir: repoRoot,
			dockerfile: "sandbox-agent/Dockerfile",
			target:     "platform",
			// The platform's layer, which the server composes under every
			// harness bundle's (ADR 26-10-16-423 §2), filled in from
			// sandbox-agent/image.json by buildImage; the placeholder marks
			// where it lands.
			metadataFile: filepath.Join(repoRoot, "sandbox-agent", "image.json"),
			metadataArg:  harness.LayerMetadataBuildArg,
			bundle:       true,
			layoutDir:    bundleLayoutDirOf(repoRoot, "discobox-platform"),
			files:        sortedFiles(platformSeen),
		},
	}
	for _, harnessImage := range harnessImages {
		harnessDir := filepath.Join("harness", harnessImage.dir)
		// The whole directory: it is the harness's feature, whose every file
		// the bundle may carry (provision.yaml names them).
		bundleSeen := map[string]struct{}{}
		if err := addTree(filepath.Join(repoRoot, harnessDir), bundleSeen); err != nil {
			return nil, err
		}
		// Every harness has one, `shell` included: a bundle names the platform
		// API version it runs on (ADR 26-10-16-423 §2).
		metadataFile := filepath.Join(repoRoot, harnessDir, "image.json")
		if _, err := os.Stat(metadataFile); err != nil {
			return nil, fmt.Errorf("harness %s: %w", harnessImage.name, err)
		}
		bundleRef := "discobox-harness-" + harnessImage.name
		// The bundle: FROM scratch, so the harness directory is all it is
		// built from.
		specs = append(specs, imageSpec{
			name:      "harness-" + harnessImage.name,
			devPrefix: bundleRef + ":dev-", buildDir: repoRoot,
			envImageKey: harnessBundleEnvKey(harnessImage.name),
			// The metadata argument is filled in from image.json by buildImage
			// at build time; the placeholder marks where it lands.
			buildArgs: bundleBuildArgs(builder, bundleLayoutDirOf(repoRoot, bundleRef), "-f", filepath.Join(harnessDir, "Dockerfile"), "--target", "bundle",
				"--build-arg", harness.MetadataBuildArg+"=", harnessDir),
			metadataFile: metadataFile,
			metadataArg:  harness.MetadataBuildArg,
			// The harness build context is the harness directory itself, so its
			// Dockerfile is at the context root.
			contextDir: filepath.Join(repoRoot, harnessDir),
			dockerfile: "Dockerfile",
			target:     "bundle",
			bundle:     true,
			layoutDir:  bundleLayoutDirOf(repoRoot, bundleRef),
			files:      sortedFiles(bundleSeen),
		})
	}
	return specs, nil
}

// bundleLayoutDirOf is the directory the bundle named name is exported into.
func bundleLayoutDirOf(repoRoot, name string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(bundleLayoutDir), name)
}

// partialLayout is where a bundle's export is written before its digest, and
// so its name, is known.
func partialLayout(layoutDir string) string {
	return filepath.Join(layoutDir, ".partial.tar")
}

// digestLayout is the layout the bundle pinned by digest is kept at. Named by
// its content, a rebuild never rewrites the layout a published manifest names:
// that one is served until the manifest naming its successor is published,
// whatever fails in between (pruneLayouts).
func digestLayout(layoutDir, digest string) string {
	return filepath.Join(layoutDir, strings.TrimPrefix(digest, "sha256:")+".tar")
}

// bundleBuildArgs is the docker argv exporting a bundle's stage into
// layoutDir, an OCI image layout tar, by way of partialLayout. No image store
// is involved: a bundle is applied by the sandbox agent, never run (ADR
// 26-10-16-423 §13). Neither provenance nor an SBOM is attached, so the
// layout's index lists the one manifest the bundle is pinned by. builder is
// the one localBundleBuilder chose; empty, in build mode, names none.
func bundleBuildArgs(builder, layoutDir string, args ...string) []string {
	argv := []string{"buildx", "build"}
	if builder != "" {
		argv = append(argv, "--builder", builder)
	}
	return append(append(argv, "--provenance=false", "--sbom=false",
		"--output", "type=oci,dest="+partialLayout(layoutDir)), args...)
}

// harnessBundleEnvKey returns the .env key the server reads to override a
// harness definition's bundle. The name equals the definition's slug. Keep in
// sync with the server-side harnessdefs mapping (DISCOBOX_HARNESS_<SLUG>_BUNDLE).
func harnessBundleEnvKey(harnessName string) string {
	return "DISCOBOX_HARNESS_" + harnessEnvSlug(harnessName) + "_BUNDLE"
}

func harnessEnvSlug(harnessName string) string {
	return strings.ToUpper(strings.ReplaceAll(harnessName, "-", "_"))
}

// harnessMetadata reads an authoring-time image.json and compacts it wholesale
// for its build-arg: a manifest label's payload is the full image.json shape
// (apiVersion, env, volumes, harness), not just the harness sub-object. The
// same function serves a harness bundle's own layer and the platform bundle's
// — they are the same shape, differing only in which label key carries them
// (ADR 0086 §2).
func harnessMetadata(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	compact := bytes.Buffer{}
	if err := json.Compact(&compact, data); err != nil {
		return "", fmt.Errorf("read harness metadata from %s: %w", path, err)
	}
	return compact.String(), nil
}

// goModuleFiles lists every Go source file the named packages are built from.
//
// The dependency scope is the whole repository, not just the module being
// built: these binaries import root-module packages (layout, proxy,
// sandboxconfig, ...) that their Dockerfiles copy into the build context, and a
// change to one of those changes the image just as surely as a change inside
// the module. Scoping the scan to the module directory would leave the
// content-addressed image reference unchanged after such an edit, so the stale
// image would never be rebuilt.
//
// Deriving the set from `go list -deps` rather than a hand-written list is the
// point: a newly added import is picked up automatically, where a hand-written
// list silently goes stale exactly when it matters.
func goModuleFiles(ctx context.Context, moduleRoot, repoRoot string, packages ...string) ([]string, error) {
	args := append([]string{"list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}"}, packages...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list deps in %s: %w", moduleRoot, err)
	}
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		dir := strings.TrimSpace(scanner.Text())
		if dir == "" || !inside(repoRoot, dir) {
			continue
		}
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "build" || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				seen[path] = struct{}{}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sortedFiles(seen), nil
}

func addTree(root string, seen map[string]struct{}) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			case "build":
				// A build output at the tree's top, never a directory of
				// that name within it, such as the platform's build feature.
				if filepath.Dir(path) == root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		seen[path] = struct{}{}
		return nil
	})
}

func sortedFiles(seen map[string]struct{}) []string {
	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

func addFile(root, rel string, seen map[string]struct{}) {
	path := filepath.Join(root, rel)
	if _, err := os.Stat(path); err == nil {
		seen[path] = struct{}{}
	}
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}

type fileState struct {
	modTime time.Time
	size    int64
}

func snapshot(files []string) map[string]fileState {
	state := make(map[string]fileState, len(files))
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		state[file] = fileState{modTime: info.ModTime(), size: info.Size()}
	}
	return state
}

func changed(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return true
	}
	for file, old := range a {
		if next, ok := b[file]; !ok || !next.modTime.Equal(old.modTime) || next.size != old.size {
			return true
		}
	}
	return false
}

func buildChangedImages(ctx context.Context, repoRoot string, allSpecs, changedSpecs []imageSpec) error {
	if buildModeEnabled() {
		// Nothing is built here: the host has no Docker daemon to build on, so
		// the manifest describes the builds and each destination daemon runs
		// them. Stamping is cheap, so it always covers every spec.
		return stampBuildModeImages(repoRoot, allSpecs)
	}
	if changedSpecs == nil {
		changedSpecs = allSpecs
	}
	// The pass publishes every bundle, so one the last manifest does not
	// name a layout for is exported in it, whatever changed.
	published := publishedBundles(repoRoot)
	for _, spec := range allSpecs {
		if _, ok := publishedBundle(spec, published); spec.bundle && !ok && !containsSpec(changedSpecs, spec.name) {
			changedSpecs = append(changedSpecs, spec)
		}
	}
	exported := map[string]devimage.Bundle{}
	for _, spec := range changedSpecs {
		if spec.bundle {
			bundle, err := buildBundle(ctx, spec)
			if err != nil {
				return err
			}
			exported[spec.name] = bundle
			continue
		}
		if err := buildImage(ctx, spec); err != nil {
			return err
		}
	}
	manifest, err := developmentImages(ctx, repoRoot, allSpecs, exported, published)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(repoRoot, developmentImageManifestFile)
	if err := devimage.WriteAtomic(manifestPath, manifest); err != nil {
		return fmt.Errorf("write development image manifest: %w", err)
	}
	values, err := developmentImageEnv(allSpecs, manifest, manifestPath)
	if err != nil {
		return err
	}
	if err := updateEnv(filepath.Join(repoRoot, envFile), values); err != nil {
		return err
	}
	pruneLayouts(allSpecs, manifest.Bundles, published)
	return nil
}

// pruneLayouts removes every bundle layout neither the manifest just
// published nor the one it replaced names. The replaced one is kept for the
// server still running on it until it restarts on the new one; only once a
// pass has published its successor is it given up.
func pruneLayouts(specs []imageSpec, current, previous []devimage.Bundle) {
	keep := map[string]bool{}
	for _, bundle := range append(append([]devimage.Bundle(nil), current...), previous...) {
		keep[bundle.Layout] = true
	}
	for _, spec := range specs {
		if !spec.bundle {
			continue
		}
		layouts, err := filepath.Glob(filepath.Join(spec.layoutDir, "*.tar"))
		if err != nil {
			continue
		}
		for _, layout := range layouts {
			if !keep[layout] && layout != partialLayout(spec.layoutDir) {
				if err := os.Remove(layout); err != nil {
					log.Printf("remove superseded bundle layout %s: %v", layout, err)
				}
			}
		}
	}
}

// developmentImageEnv derives every published reference from the complete
// manifest just written, its bundles included. A partial rebuild
// must not update only its own keys: another process may have moved any
// mutable :local tag since the previous pass, and publishing that complete
// snapshot in the manifest while retaining an old .env reference gives the
// server two different image sets.
func developmentImageEnv(specs []imageSpec, manifest devimage.Manifest, manifestPath string) (map[string]string, error) {
	values := map[string]string{
		devimage.SyncEnv:     "true",
		devimage.ManifestEnv: manifestPath,
	}
	used := make(map[int]bool, len(manifest.Images))
	usedBundles := make(map[int]bool, len(manifest.Bundles))
	for _, spec := range specs {
		if spec.bundle {
			bundle := -1
			for i, entry := range manifest.Bundles {
				if strings.HasPrefix(entry.Reference, spec.devPrefix) {
					if bundle >= 0 {
						return nil, fmt.Errorf("development image manifest has multiple bundles for %s", spec.name)
					}
					bundle = i
				}
			}
			if bundle < 0 {
				return nil, fmt.Errorf("development image manifest has no bundle for %s", spec.name)
			}
			usedBundles[bundle] = true
			if spec.envImageKey != "" {
				values[spec.envImageKey] = manifest.Bundles[bundle].Reference
			}
			continue
		}
		image := -1
		for i, entry := range manifest.Images {
			if strings.HasPrefix(entry.Reference, spec.devPrefix) {
				if image >= 0 {
					return nil, fmt.Errorf("development image manifest has multiple images for %s", spec.name)
				}
				image = i
			}
		}
		if image < 0 {
			return nil, fmt.Errorf("development image manifest has no image for %s", spec.name)
		}
		entry := manifest.Images[image]
		used[image] = true
		if spec.envImageKey != "" {
			values[spec.envImageKey] = entry.Reference
		}
	}
	if len(used) != len(manifest.Images) {
		return nil, fmt.Errorf("development image manifest has %d unexpected images", len(manifest.Images)-len(used))
	}
	if len(usedBundles) != len(manifest.Bundles) {
		return nil, fmt.Errorf("development image manifest has %d unexpected bundles", len(manifest.Bundles)-len(usedBundles))
	}
	return values, nil
}

// developmentImages inspects everything built: the images copied to a
// destination daemon, and the bundles the server serves from their layouts —
// exported in this pass, or else as published before it.
func developmentImages(ctx context.Context, repoRoot string, specs []imageSpec, exported map[string]devimage.Bundle, published []devimage.Bundle) (devimage.Manifest, error) {
	images := make([]devimage.Image, 0, len(specs))
	var bundles []devimage.Bundle
	for _, spec := range specs {
		// A bundle is not run: the server serves it from its layout, and a
		// pool runs the harness's twin until the agent applies bundles.
		if spec.bundle {
			bundle, ok := exported[spec.name]
			if !ok {
				bundle, ok = publishedBundle(spec, published)
			}
			if !ok {
				return devimage.Manifest{}, fmt.Errorf("no layout exported for development bundle %s", spec.name)
			}
			bundles = append(bundles, bundle)
			continue
		}
		imageID, err := commandOutput(ctx, repoRoot, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
		if err != nil {
			return devimage.Manifest{}, fmt.Errorf("inspect development image %s: %w", spec.baseImage, err)
		}
		imageID = strings.TrimSpace(imageID)
		images = append(images, devimage.Image{
			Reference: devImageTag(spec.devPrefix, imageID),
			ID:        imageID,
		})
	}
	return devimage.NewManifest(images, bundles)
}

// renderBuildArgs fills in the placeholders a spec's docker argv carries: the
// manifest metadata read from image.json. A missed metadata argument fails the
// build only because the Dockerfiles refuse an empty one, so this is separated
// from the docker invocation to be assertable on its own.
func renderBuildArgs(spec imageSpec) ([]string, error) {
	buildArgs := append([]string{}, spec.buildArgs...)
	replace := func(prefix, value string) {
		for i, arg := range buildArgs {
			if strings.HasPrefix(arg, prefix) {
				buildArgs[i] = prefix + value
			}
		}
	}
	if spec.metadataFile != "" {
		metadata, err := harnessMetadata(spec.metadataFile)
		if err != nil {
			return nil, err
		}
		replace(spec.metadataArg+"=", metadata)
	}
	return buildArgs, nil
}

// bundleOutputArgs build a bundle with every file's time clamped to one
// epoch. A box fetches only the layers its store lacks (ADR 26-10-16-423
// §7), and a layer's bytes carry its files' times: a binary rebuilt the same
// or a file checked out again would otherwise be a new layer every box
// fetches again.
var bundleOutputArgs = []string{"--output", "type=docker,rewrite-timestamp=true", "--build-arg", "SOURCE_DATE_EPOCH=" + harness.BundleSourceDateEpoch}

func buildImage(ctx context.Context, spec imageSpec) error {
	buildArgs, err := renderBuildArgs(spec)
	if err != nil {
		return err
	}
	if spec.bundle {
		buildArgs = append(buildArgs[:1:1], append(bundleOutputArgs, buildArgs[1:]...)...)
	}
	if err := runCommand(ctx, spec.buildDir, "docker", buildArgs...); err != nil {
		return err
	}
	imageID, err := commandOutput(ctx, spec.buildDir, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
	if err != nil {
		return err
	}
	imageID = strings.TrimSpace(imageID)
	// The hashed dev tag is the reference the manifest publishes, so a
	// destination copies this exact build rather than whatever :local names by
	// then.
	image := devImageTag(spec.devPrefix, imageID)
	if err := runCommand(ctx, spec.buildDir, "docker", "tag", spec.baseImage, image); err != nil {
		return err
	}
	log.Printf("built %s as %s (%s)", spec.name, image, imageID)
	return nil
}

// buildBundle exports a bundle's stage to the layout its digest names, which
// it is renamed to only once the export is whole.
func buildBundle(ctx context.Context, spec imageSpec) (devimage.Bundle, error) {
	buildArgs, err := renderBuildArgs(spec)
	if err != nil {
		return devimage.Bundle{}, err
	}
	if err := os.MkdirAll(spec.layoutDir, 0o755); err != nil {
		return devimage.Bundle{}, fmt.Errorf("create %s: %w", spec.layoutDir, err)
	}
	partial := partialLayout(spec.layoutDir)
	defer func() { _ = os.Remove(partial) }()
	if err := runCommand(ctx, spec.buildDir, "docker", buildArgs...); err != nil {
		return devimage.Bundle{}, err
	}
	digest, err := devimage.LayoutDigest(partial)
	if err != nil {
		return devimage.Bundle{}, err
	}
	bundle := devimage.Bundle{
		Reference: devImageTag(spec.devPrefix, digest),
		Digest:    digest,
		Layout:    digestLayout(spec.layoutDir, digest),
	}
	if err := os.Rename(partial, bundle.Layout); err != nil {
		return devimage.Bundle{}, err
	}
	log.Printf("built %s as %s (%s)", spec.name, bundle.Layout, digest)
	return bundle, nil
}

// devImageTag derives the content-hashed dev tag from a docker image ID, or a
// bundle's manifest digest.
func devImageTag(prefix, imageID string) string {
	shortID := strings.TrimPrefix(imageID, "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return prefix + shortID
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func commandOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func updateEnv(path string, values map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := []string{}
	if len(data) > 0 {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	seen := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.Contains(line, "=") {
			continue
		}
		key := strings.TrimSpace(strings.SplitN(line, "=", 2)[0])
		if value, ok := values[key]; ok {
			lines[i] = key + "=" + value
			seen[key] = true
		}
	}
	missing := make([]string, 0, len(values))
	for _, key := range sortedKeys(values) {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	if len(lines) > 0 && len(missing) > 0 {
		lines = append(lines, "")
	}
	for _, key := range missing {
		lines = append(lines, key+"="+values[key])
	}
	//nolint:gosec // Development watcher writes a repository .env file meant to be user-editable.
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
