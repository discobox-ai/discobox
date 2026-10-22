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

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
)

const envFile = ".env"
const developmentImageManifestFile = ".tmp/discobox-dev-images.json"

var bundleBuilder = flag.String("bundle-builder", "discobox", "Buildx builder for local OCI bundle exports")

// bundleLayoutDir is where the watcher exports each bundle, as the OCI image
// layout tar <name>/<hex>.tar named by the digest of the manifest it holds,
// for the server to serve from (ADR 26-10-16-423 §13).
const bundleLayoutDir = "build/assets"

// baseSpecName is the Docker driver's sandbox base image, which both agent
// images build FROM via the BASE_IMAGE build arg.
const baseSpecName = "sandbox-base"

// sandboxAgentSpecName is the spec whose built image every harness's runnable
// twin layers on top of via the SANDBOX_AGENT_IMAGE build arg.
const sandboxAgentSpecName = "sandbox-agent"

// platformSpecName is the platform bundle: the sandbox agent Dockerfile's
// FROM scratch stage holding every platform feature (ADR 26-10-16-423 §7).
const platformSpecName = "platform"

// platformBundleEnvKey is the .env key the server reads the platform bundle
// from. Keep in sync with the server's DISCOBOX_PLATFORM_BUNDLE setting.
const platformBundleEnvKey = "DISCOBOX_PLATFORM_BUNDLE"

type imageSpec struct {
	name         string
	baseImage    string
	devPrefix    string
	envImageKey  string
	envDigestKey string
	buildDir     string
	buildArgs    []string
	files        []string
	// metadataFile is the authoring-time image.json compacted into metadataArg
	// at build time, which the Dockerfile turns into a manifest label. Both are
	// empty for an image whose build labels no manifest.
	metadataFile string
	metadataArg  string
	// contextDir, dockerfile and target describe the same build as buildArgs,
	// but declaratively, for build-mode: the server builds these on the
	// destination Docker daemon, so there is no docker CLI invocation to derive
	// them from. target is the stage, for a Dockerfile with more than one
	// artifact: the sandbox agent's image and the platform bundle, a harness's
	// bundle and its runnable twin.
	contextDir string
	dockerfile string
	target     string
	// parent names the spec whose built image this one builds FROM, and
	// parentArg the build argument that carries it. The parent's build-arg value
	// is rewritten to the parent's hashed dev tag so a child pins the exact base
	// it was built on rather than the mutable :local tag. The chain is
	// base -> pool-agent/sandbox-agent -> harness twin.
	parent    string
	parentArg string
	// bundle marks a FROM scratch bundle (ADR 26-10-16-423 §2): never run, so
	// copy-mode exports it into layoutDir, as an OCI image layout tar named by
	// its digest, rather than into any Docker daemon, and records it among the
	// manifest's bundles, which the server serves and reads its labels from.
	// Build-mode describes it like any other image, since there the server
	// reconstructs its labels from the manifest's build arguments.
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
	specs, err := dockerImageSpecs(ctx, repoRoot)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return errors.New("no Docker images configured")
	}
	// Only local builds need host Docker. macOS and Windows default to
	// describing builds for the pool, and Linux can opt into that mode too.
	if !buildModeEnabled() {
		if err := runCommand(ctx, repoRoot, "go", "tool", "task", "buildx:builder", "BUILDX_BUILDER="+*bundleBuilder); err != nil {
			return fmt.Errorf("prepare bundle builder: %w", err)
		}
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
	rediscover := func(ctx context.Context) ([]imageSpec, error) { return dockerImageSpecs(ctx, repoRoot) }
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
	missing := map[string]bool{}
	for _, spec := range specs {
		if !present(spec) {
			missing[spec.name] = true
		}
	}
	// A rebuilt image is a new base, so everything layered on it has to be
	// rebuilt too — otherwise the manifest would publish an image built on a
	// base that no longer exists. Swept to a fixed point rather than once,
	// because the chain is three deep: a missing base takes both agents with it,
	// and the sandbox agent takes every harness.
	for {
		grew := false
		for _, spec := range specs {
			if spec.parent != "" && missing[spec.parent] && !missing[spec.name] {
				missing[spec.name] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	out := make([]imageSpec, 0, len(missing))
	for _, spec := range specs {
		if missing[spec.name] {
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

func dockerImageSpecs(ctx context.Context, repoRoot string) ([]imageSpec, error) {
	// The sandbox base image both agent images build FROM. Its inputs are
	// folded into theirs below, so a base edit rebuilds them and changes their
	// content tags rather than leaving them on a base that no longer exists.
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
	for file := range baseSeen {
		workerSeen[file] = struct{}{}
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
	for _, file := range sandboxFiles {
		sandboxSeen[file] = struct{}{}
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
	// its inputs; the runnable image and everything on it are built FROM it.
	platformSeen := copyFiles(sandboxSeen)
	for file := range baseSeen {
		sandboxSeen[file] = struct{}{}
	}
	commonSandboxSeen := copyFiles(sandboxSeen)
	for _, harnessImage := range harnessImages {
		for _, name := range []string{"Dockerfile", "configure.sh", "image.json"} {
			delete(commonSandboxSeen, filepath.Join(repoRoot, "harness", harnessImage.dir, name))
		}
	}
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
			name:         "pool-agent",
			baseImage:    "discobox-pool-agent:local",
			devPrefix:    "discobox-pool-agent:dev-",
			envImageKey:  "DISCOBOX_DOCKER_POOL_IMAGE",
			envDigestKey: "DISCOBOX_DOCKER_POOL_IMAGE_DIGEST",
			buildDir:     repoRoot,
			buildArgs: []string{"build", "-f", "pool-agent/Dockerfile",
				"--build-arg", "BASE_IMAGE=discobox-sandbox-base:local",
				"-t", "discobox-pool-agent:local", "."},
			contextDir: repoRoot,
			dockerfile: "pool-agent/Dockerfile",
			parent:     baseSpecName,
			parentArg:  "BASE_IMAGE",
			files:      sortedFiles(workerSeen),
		},
		{
			name:         sandboxAgentSpecName,
			baseImage:    "discobox-sandbox-agent:local",
			devPrefix:    "discobox-sandbox-agent:dev-",
			envImageKey:  "DISCOBOX_DEFAULT_SANDBOX_IMAGE",
			envDigestKey: "DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST",
			buildDir:     repoRoot,
			// The runnable stage by name: the Dockerfile's default target is
			// the platform bundle, which has a spec of its own below.
			buildArgs: []string{"build", "-f", "sandbox-agent/Dockerfile", "--target", "sandbox-agent",
				"--build-arg", "BASE_IMAGE=discobox-sandbox-base:local",
				"--build-arg", harness.LayerMetadataBuildArg + "=",
				"-t", "discobox-sandbox-agent:local", "."},
			contextDir: repoRoot,
			dockerfile: "sandbox-agent/Dockerfile",
			target:     "sandbox-agent",
			parent:     baseSpecName,
			parentArg:  "BASE_IMAGE",
			// The platform's layer, labeled on the runnable image too so
			// that the digest a sandbox pins moves with every edit to it;
			// nothing reads it off this image.
			metadataFile: filepath.Join(repoRoot, "sandbox-agent", "image.json"),
			metadataArg:  harness.LayerMetadataBuildArg,
			files:        sortedFiles(commonSandboxSeen),
		},
		{
			name:        platformSpecName,
			devPrefix:   "discobox-platform:dev-",
			envImageKey: platformBundleEnvKey,
			buildDir:    repoRoot,
			buildArgs: bundleBuildArgs(bundleLayoutDirOf(repoRoot, "discobox-platform"), "-f", "sandbox-agent/Dockerfile", "--target", "platform",
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
		imageRef := bundleRef + "-image"
		// The bundle: FROM scratch, so the harness directory is all it is
		// built from.
		specs = append(specs, imageSpec{
			name:      "harness-" + harnessImage.name,
			devPrefix: bundleRef + ":dev-", buildDir: repoRoot,
			envImageKey: harnessBundleEnvKey(harnessImage.name),
			// The metadata argument is filled in from image.json by buildImage
			// at build time; the placeholder marks where it lands.
			buildArgs: bundleBuildArgs(bundleLayoutDirOf(repoRoot, bundleRef), "-f", filepath.Join(harnessDir, "Dockerfile"), "--target", "bundle",
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
		// The runnable twin a pool runs until the agent applies bundles
		// itself: the bundle's features provisioned onto the sandbox agent
		// image. It takes the metadata argument too: it labels it, so the
		// digest a sandbox pins moves with every manifest edit, and it builds
		// the bundle stage it copies the features from.
		seen := copyFiles(commonSandboxSeen)
		for file := range bundleSeen {
			seen[file] = struct{}{}
		}
		specs = append(specs, imageSpec{
			name: "harness-" + harnessImage.name + "-image", baseImage: imageRef + ":local",
			devPrefix: imageRef + ":dev-", buildDir: repoRoot,
			// Mirror the worker/sandbox flow: write the hashed dev tag to .env so the
			// server restarts and resolves the new harness image.
			envImageKey: harnessImageEnvKey(harnessImage.name),
			buildArgs: []string{"build", "-f", filepath.Join(harnessDir, "Dockerfile"), "--target", "image",
				"--build-arg", "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local",
				"--build-arg", harness.MetadataBuildArg + "=",
				"-t", imageRef + ":local", harnessDir},
			metadataFile: metadataFile,
			metadataArg:  harness.MetadataBuildArg,
			parent:       sandboxAgentSpecName,
			parentArg:    "SANDBOX_AGENT_IMAGE",
			contextDir:   filepath.Join(repoRoot, harnessDir),
			dockerfile:   "Dockerfile",
			target:       "image",
			files:        sortedFiles(seen),
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
// layout's index lists the one manifest the bundle is pinned by. The OCI
// exporter uses the docker-container builder prepared at watcher startup,
// independently of the host daemon's image store and runnable-image builds.
func bundleBuildArgs(layoutDir string, args ...string) []string {
	return append([]string{"buildx", "build", "--builder", *bundleBuilder, "--provenance=false", "--sbom=false",
		"--output", "type=oci,dest=" + partialLayout(layoutDir)}, args...)
}

// harnessImageEnvKey and harnessBundleEnvKey return the .env keys the server
// reads to override a harness definition's runnable image and its bundle. The
// image name equals the definition's slug. Keep in sync with the server-side
// harnessdefs mapping (DISCOBOX_HARNESS_<SLUG>_IMAGE and _BUNDLE).
func harnessImageEnvKey(harnessName string) string {
	return "DISCOBOX_HARNESS_" + harnessEnvSlug(harnessName) + "_IMAGE"
}

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

func copyFiles(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for file := range in {
		out[file] = struct{}{}
	}
	return out
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
	// An image is built after whatever it builds FROM, and is handed that
	// parent's content-hashed dev tag rather than the mutable :local one. A child
	// can change on its own while its parent is unchanged; in that case the
	// parent's current dev tag is resolved from its built :local image.
	ordered := parentsFirst(changedSpecs)
	built := map[string]string{}
	exported := map[string]devimage.Bundle{}
	for _, spec := range ordered {
		parentImage := ""
		if spec.parent != "" {
			resolved, ok := built[spec.parent]
			if !ok {
				var err error
				resolved, err = resolveParentImage(ctx, repoRoot, allSpecs, spec.parent)
				if err != nil {
					return err
				}
			}
			parentImage = resolved
		}
		if spec.bundle {
			// Nothing builds FROM a bundle, so it has no tag to hand on.
			bundle, err := buildBundle(ctx, spec)
			if err != nil {
				return err
			}
			exported[spec.name] = bundle
			continue
		}
		image, _, err := buildImage(ctx, spec, parentImage)
		if err != nil {
			return err
		}
		built[spec.name] = image
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
		if spec.envDigestKey != "" {
			values[spec.envDigestKey] = entry.ID
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

// parentsFirst returns specs ordered so an image is built after the image it
// builds FROM. A spec whose parent is not in this set is ready immediately —
// that is the pass that rebuilds one harness while its base is unchanged.
func parentsFirst(specs []imageSpec) []imageSpec {
	ordered := make([]imageSpec, 0, len(specs))
	placed := make(map[string]bool, len(specs))
	// The graph is a shallow tree (base -> agents -> harnesses), so sweeping
	// until nothing new can be placed is enough and needs no cycle bookkeeping.
	for len(ordered) < len(specs) {
		progressed := false
		for _, spec := range specs {
			if placed[spec.name] {
				continue
			}
			if spec.parent != "" && containsSpec(specs, spec.parent) && !placed[spec.parent] {
				continue
			}
			ordered = append(ordered, spec)
			placed[spec.name] = true
			progressed = true
		}
		if progressed {
			continue
		}
		// Only reachable from a cycle in the parent links, which would otherwise
		// spin here forever. Nothing orders the remainder, so keep spec order.
		for _, spec := range specs {
			if !placed[spec.name] {
				ordered = append(ordered, spec)
				placed[spec.name] = true
			}
		}
	}
	return ordered
}

// resolveParentImage returns the named spec's current dev tag by inspecting its
// built :local image, for passes that rebuild a child without rebuilding what it
// is built FROM.
//
// The tag is applied, not just derived. Only buildImage creates a dev tag, and
// only for a build this watcher ran; anything else that rebuilds the base in
// place — task build:sandbox-base-image, build:images, the Dockerfile hook — moves
// :local to an image ID no dev tag names. A derived-but-absent tag is not a
// local miss docker reports: it is a repository on Docker Hub, and the child's
// FROM goes off to pull it. Tagging is exact rather than approximate, since the
// ID came from :local itself, and is idempotent for the tag that already exists.
func resolveParentImage(ctx context.Context, repoRoot string, specs []imageSpec, name string) (string, error) {
	for _, spec := range specs {
		if spec.name != name {
			continue
		}
		imageID, err := commandOutput(ctx, repoRoot, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
		if err != nil {
			return "", fmt.Errorf("resolve %s base image: %w", spec.name, err)
		}
		image := devImageTag(spec.devPrefix, strings.TrimSpace(imageID))
		if err := runCommand(ctx, repoRoot, "docker", "tag", spec.baseImage, image); err != nil {
			return "", fmt.Errorf("tag %s as %s: %w", spec.baseImage, image, err)
		}
		return image, nil
	}
	return "", fmt.Errorf("no %s spec configured", name)
}

// renderBuildArgs fills in the placeholders a spec's docker argv carries: the
// manifest metadata read from image.json, and the reference of the image this
// one builds FROM. A missed parent fails quietly — the build succeeds against
// the mutable :local tag — and a missed metadata argument fails the build
// only because the Dockerfiles refuse an empty one, so this is separated from
// the docker invocation to be assertable on its own.
func renderBuildArgs(spec imageSpec, parentImage string) ([]string, error) {
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
	if spec.parentArg != "" && parentImage != "" {
		replace(spec.parentArg+"=", parentImage)
	}
	return buildArgs, nil
}

func buildImage(ctx context.Context, spec imageSpec, parentImage string) (string, string, error) {
	buildArgs, err := renderBuildArgs(spec, parentImage)
	if err != nil {
		return "", "", err
	}
	if err := runCommand(ctx, spec.buildDir, "docker", buildArgs...); err != nil {
		return "", "", err
	}
	imageID, err := commandOutput(ctx, spec.buildDir, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
	if err != nil {
		return "", "", err
	}
	imageID = strings.TrimSpace(imageID)
	// Every image gets the hashed dev tag, including one nothing runs: it is what
	// a child's FROM pins, so a base that is rebuilt in place cannot be silently
	// swapped under an image already built on it.
	image := devImageTag(spec.devPrefix, imageID)
	if err := runCommand(ctx, spec.buildDir, "docker", "tag", spec.baseImage, image); err != nil {
		return "", "", err
	}
	log.Printf("built %s as %s (%s)", spec.name, image, imageID)
	return image, imageID, nil
}

// buildBundle exports a bundle's stage to the layout its digest names, which
// it is renamed to only once the export is whole.
func buildBundle(ctx context.Context, spec imageSpec) (devimage.Bundle, error) {
	buildArgs, err := renderBuildArgs(spec, "")
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
