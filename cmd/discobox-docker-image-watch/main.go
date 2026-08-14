package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/obot-platform/discobox/devimage"
)

const envFile = ".env"
const developmentImageManifestFile = ".tmp/discobox-dev-images.json"

// sandboxAgentSpecName is the spec whose built image every harness layers on
// top of via the SANDBOX_AGENT_IMAGE build arg.
const sandboxAgentSpecName = "sandbox-agent"

type imageSpec struct {
	name         string
	baseImage    string
	devPrefix    string
	envImageKey  string
	envDigestKey string
	buildDir     string
	buildArgs    []string
	files        []string
	metadataFile string
	// contextDir and dockerfile describe the same build as buildArgs, but
	// declaratively, for build-mode: the server builds these on the destination
	// Docker daemon, so there is no docker CLI invocation to derive them from.
	contextDir string
	dockerfile string
	// sandboxBase marks specs that build FROM the sandbox-agent image. Their
	// SANDBOX_AGENT_IMAGE build arg is rewritten to the sandbox-agent dev tag so
	// they pin the exact hashed base rather than the mutable :local tag.
	sandboxBase bool
}

type harnessImage struct {
	name string
	dir  string
}

var harnessImages = []harnessImage{
	{name: "codex", dir: "codex-cli"},
	{name: "claude-code", dir: "claude-code"},
	{name: "opencode", dir: "opencode"},
}

func main() {
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
	states := make(map[string]map[string]fileState, len(specs))
	for _, spec := range specs {
		if len(spec.files) == 0 {
			return fmt.Errorf("no Docker inputs discovered for %s", spec.name)
		}
		log.Printf("watching %d Docker inputs for %s", len(spec.files), spec.name)
		states[spec.name] = snapshot(spec.files)
	}
	if err := buildChangedImages(ctx, repoRoot, specs, nil); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// A separate, slower beat for "is the image still there", which costs a
	// Docker call where the file check costs a stat.
	presence := time.NewTicker(missingImageCheckInterval)
	defer presence.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-presence.C:
			missing, err := missingImageSpecs(ctx, repoRoot, specs)
			if err != nil {
				log.Printf("check built images: %v", err)
				continue
			}
			if len(missing) == 0 {
				continue
			}
			log.Printf("rebuilding %s: built image no longer on the daemon", strings.Join(specNames(missing), ", "))
			if err := buildChangedImages(ctx, repoRoot, specs, missing); err != nil {
				log.Printf("build failed: %v", err)
			}
		case <-ticker.C:
			var changedSpecs []imageSpec
			for _, spec := range specs {
				next := snapshot(spec.files)
				if changed(states[spec.name], next) {
					states[spec.name] = next
					changedSpecs = append(changedSpecs, spec)
				}
			}
			if len(changedSpecs) == 0 {
				continue
			}
			log.Printf("Docker inputs changed for %s; rebuilding", strings.Join(specNames(changedSpecs), ", "))
			if err := buildChangedImages(ctx, repoRoot, specs, changedSpecs); err != nil {
				log.Printf("build failed: %v", err)
			}
		}
	}
}

// missingImageCheckInterval paces the check for built images that have left the
// daemon.
const missingImageCheckInterval = 15 * time.Second

// missingImageSpecs returns the specs whose built image is no longer on the
// daemon.
//
// Rebuilding on a file change alone is not enough, because a built image can
// leave without any file changing: image reclamation removes a superseded one
// (ADR 0039), and a developer's own `docker system prune` removes all of them.
// Nothing then rebuilds it, while `.env` and the manifest keep naming it, so
// every pool reconcile fails against an image that cannot come back. This is the
// level-triggered half the watcher was missing: what it published must still
// exist, not merely have existed once.
func missingImageSpecs(ctx context.Context, repoRoot string, specs []imageSpec) ([]imageSpec, error) {
	if buildModeEnabled() {
		// Nothing is built on this host, so there is no local image to miss.
		return nil, nil
	}
	present, err := commandOutput(ctx, repoRoot, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	tags := map[string]struct{}{}
	for _, tag := range strings.Fields(present) {
		tags[tag] = struct{}{}
	}
	return missingFrom(specs, tags), nil
}

// missingFrom is the decision behind missingImageSpecs, over the set of image
// references the daemon reports.
func missingFrom(specs []imageSpec, present map[string]struct{}) []imageSpec {
	missing := make([]imageSpec, 0, len(specs))
	for _, spec := range specs {
		if _, ok := present[spec.baseImage]; !ok {
			missing = append(missing, spec)
		}
	}
	// A rebuilt sandbox-agent is a new base, so every image layered on it has to
	// be rebuilt too — otherwise the manifest would publish harness images built
	// on a base that no longer exists.
	if !containsSpec(missing, sandboxAgentSpecName) {
		return missing
	}
	for _, spec := range specs {
		if spec.sandboxBase && !containsSpec(missing, spec.name) {
			missing = append(missing, spec)
		}
	}
	return missing
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
	sandboxRoot := filepath.Join(repoRoot, "sandbox-agent")
	sandboxSeen := map[string]struct{}{}
	// The whole tree, so non-Go assets (image scripts, unit files) count too.
	if err := addTree(sandboxRoot, sandboxSeen); err != nil {
		return nil, err
	}
	// Every binary the Dockerfile builds, so every root-module package it
	// copies into the build context is watched without naming any of them.
	sandboxFiles, err := goModuleFiles(ctx, sandboxRoot, repoRoot,
		"./cmd/discobox-sandbox-agent", "./cmd/discobox-runc", "./cmd/discobox-docker")
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
	commonSandboxSeen := copyFiles(sandboxSeen)
	for _, harnessImage := range harnessImages {
		for _, name := range []string{"Dockerfile", "configure.sh", "image.json"} {
			delete(commonSandboxSeen, filepath.Join(repoRoot, "harness", harnessImage.dir, name))
		}
	}
	specs := []imageSpec{
		{
			name:         "pool-agent",
			baseImage:    "discobox-pool-agent:local",
			devPrefix:    "discobox-pool-agent:dev-",
			envImageKey:  "DISCOBOX_DOCKER_POOL_IMAGE",
			envDigestKey: "DISCOBOX_DOCKER_POOL_IMAGE_DIGEST",
			buildDir:     repoRoot,
			buildArgs:    []string{"build", "-f", "pool-agent/Dockerfile", "-t", "discobox-pool-agent:local", "."},
			contextDir:   repoRoot,
			dockerfile:   "pool-agent/Dockerfile",
			files:        sortedFiles(workerSeen),
		},
		{
			name:         "sandbox-agent",
			baseImage:    "discobox-sandbox-agent:local",
			devPrefix:    "discobox-sandbox-agent:dev-",
			envImageKey:  "DISCOBOX_DEFAULT_SANDBOX_IMAGE",
			envDigestKey: "DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST",
			buildDir:     repoRoot,
			buildArgs:    []string{"build", "-f", "sandbox-agent/Dockerfile", "-t", "discobox-sandbox-agent:local", "."},
			contextDir:   repoRoot,
			dockerfile:   "sandbox-agent/Dockerfile",
			files:        sortedFiles(commonSandboxSeen),
		},
	}
	for _, harnessImage := range harnessImages {
		seen := copyFiles(commonSandboxSeen)
		harnessDir := filepath.Join("harness", harnessImage.dir)
		for _, name := range []string{"Dockerfile", "configure.sh", "image.json"} {
			addFile(repoRoot, filepath.Join(harnessDir, name), seen)
		}
		specs = append(specs, imageSpec{
			name: "harness-" + harnessImage.name, baseImage: "discobox-harness-" + harnessImage.name + ":local",
			devPrefix: "discobox-harness-" + harnessImage.name + ":dev-", buildDir: repoRoot,
			// Mirror the worker/sandbox flow: write the hashed dev tag to .env so the
			// server restarts and resolves the new harness image. The env key must
			// match the server-side harnessdefs.ImageEnvVar mapping (definition id →
			// DISCOBOX_HARNESS_<ID>_IMAGE); the image name equals the definition id.
			envImageKey: harnessImageEnvKey(harnessImage.name),
			// HARNESS_METADATA is filled in from image.json by buildImage at build
			// time; the placeholder marks where it lands.
			buildArgs: []string{"build", "-f", filepath.Join(harnessDir, "Dockerfile"),
				"--build-arg", "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local",
				"--build-arg", "HARNESS_METADATA=", "-t", "discobox-harness-" + harnessImage.name + ":local", harnessDir},
			metadataFile: filepath.Join(repoRoot, harnessDir, "image.json"),
			sandboxBase:  true,
			// The harness build context is the harness directory itself, so its
			// Dockerfile is at the context root.
			contextDir: filepath.Join(repoRoot, harnessDir),
			dockerfile: "Dockerfile",
			files:      sortedFiles(seen),
		})
	}
	return specs, nil
}

// harnessImageEnvKey returns the .env key the server reads to override a
// harness definition's image. Keep in sync with the server-side
// harnessdefs.ImageEnvVar mapping.
func harnessImageEnvKey(harnessName string) string {
	return "DISCOBOX_HARNESS_" + strings.ToUpper(strings.ReplaceAll(harnessName, "-", "_")) + "_IMAGE"
}

// harnessMetadata reads a harness's authoring-time image.json and compacts it
// wholesale for the HARNESS_METADATA build-arg: the io.discobox.image.v1
// label's payload is the full image.json shape (apiVersion, env, volumes,
// harness), not just the harness sub-object.
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
			case ".git", "build", "node_modules", "testdata":
				return filepath.SkipDir
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
	// Harnesses build FROM the sandbox-agent image, so build sandbox-agent first
	// and thread its content-hashed dev tag into their SANDBOX_AGENT_IMAGE build
	// arg. A harness can change on its own while sandbox-agent is unchanged; in
	// that case resolve the current sandbox-agent dev tag from its :local image.
	ordered := sandboxAgentFirst(changedSpecs)
	values := map[string]string{}
	sandboxImage := ""
	for _, spec := range ordered {
		if spec.sandboxBase && sandboxImage == "" {
			resolved, err := resolveSandboxAgentImage(ctx, repoRoot, allSpecs)
			if err != nil {
				return err
			}
			sandboxImage = resolved
		}
		image, imageID, err := buildImage(ctx, spec, sandboxImage)
		if err != nil {
			return err
		}
		if spec.name == sandboxAgentSpecName {
			sandboxImage = image
		}
		if spec.envImageKey != "" {
			values[spec.envImageKey] = image
		}
		if spec.envDigestKey != "" {
			values[spec.envDigestKey] = imageID
		}
	}
	manifest, err := developmentImageManifest(ctx, repoRoot, allSpecs)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(repoRoot, developmentImageManifestFile)
	if err := devimage.WriteAtomic(manifestPath, manifest); err != nil {
		return fmt.Errorf("write development image manifest: %w", err)
	}
	values[devimage.SyncEnv] = "true"
	values[devimage.ManifestEnv] = manifestPath
	return updateEnv(filepath.Join(repoRoot, envFile), values)
}

func developmentImageManifest(ctx context.Context, repoRoot string, specs []imageSpec) (devimage.Manifest, error) {
	images := make([]devimage.Image, 0, len(specs))
	for _, spec := range specs {
		if spec.envImageKey == "" {
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
	return devimage.NewManifest(images)
}

// sandboxAgentFirst returns specs ordered so sandbox-agent is built before any
// harness that layers on top of it.
func sandboxAgentFirst(specs []imageSpec) []imageSpec {
	ordered := make([]imageSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.name == sandboxAgentSpecName {
			ordered = append(ordered, spec)
		}
	}
	for _, spec := range specs {
		if spec.name != sandboxAgentSpecName {
			ordered = append(ordered, spec)
		}
	}
	return ordered
}

// resolveSandboxAgentImage returns the current sandbox-agent dev tag by
// inspecting its built :local image, for passes that rebuild a harness without
// rebuilding sandbox-agent.
func resolveSandboxAgentImage(ctx context.Context, repoRoot string, specs []imageSpec) (string, error) {
	for _, spec := range specs {
		if spec.name != sandboxAgentSpecName {
			continue
		}
		imageID, err := commandOutput(ctx, repoRoot, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
		if err != nil {
			return "", fmt.Errorf("resolve %s base image: %w", spec.name, err)
		}
		return devImageTag(spec.devPrefix, strings.TrimSpace(imageID)), nil
	}
	return "", fmt.Errorf("no %s spec configured", sandboxAgentSpecName)
}

func buildImage(ctx context.Context, spec imageSpec, sandboxImage string) (string, string, error) {
	buildArgs := append([]string{}, spec.buildArgs...)
	if spec.metadataFile != "" {
		metadata, err := harnessMetadata(spec.metadataFile)
		if err != nil {
			return "", "", err
		}
		for i, arg := range buildArgs {
			if strings.HasPrefix(arg, "HARNESS_METADATA=") {
				buildArgs[i] = "HARNESS_METADATA=" + metadata
			}
		}
	}
	if spec.sandboxBase && sandboxImage != "" {
		for i, arg := range buildArgs {
			if strings.HasPrefix(arg, "SANDBOX_AGENT_IMAGE=") {
				buildArgs[i] = "SANDBOX_AGENT_IMAGE=" + sandboxImage
			}
		}
	}
	if err := runCommand(ctx, spec.buildDir, "docker", buildArgs...); err != nil {
		return "", "", err
	}
	imageID, err := commandOutput(ctx, spec.buildDir, "docker", "image", "inspect", "-f", "{{.Id}}", spec.baseImage)
	if err != nil {
		return "", "", err
	}
	imageID = strings.TrimSpace(imageID)
	if spec.envImageKey == "" {
		log.Printf("built %s as %s (%s)", spec.name, spec.baseImage, imageID)
		return spec.baseImage, imageID, nil
	}
	image := devImageTag(spec.devPrefix, imageID)
	if err := runCommand(ctx, spec.buildDir, "docker", "tag", spec.baseImage, image); err != nil {
		return "", "", err
	}
	log.Printf("built %s as %s (%s)", spec.name, image, imageID)
	return image, imageID, nil
}

// devImageTag derives the content-hashed dev tag from a docker image ID.
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
