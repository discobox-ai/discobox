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
)

const envFile = ".env"

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
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
			names := make([]string, 0, len(changedSpecs))
			for _, spec := range changedSpecs {
				names = append(names, spec.name)
			}
			log.Printf("Docker inputs changed for %s; rebuilding", strings.Join(names, ", "))
			if err := buildChangedImages(ctx, repoRoot, specs, changedSpecs); err != nil {
				log.Printf("build failed: %v", err)
			}
		}
	}
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "Taskfile.yml")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "worker-agent", "go.mod")); err == nil {
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
	workerRoot := filepath.Join(repoRoot, "worker-agent")
	workerFiles, err := workerAgentFiles(ctx, workerRoot)
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
	if err := addTree(sandboxRoot, sandboxSeen); err != nil {
		return nil, err
	}
	for _, rel := range []string{
		".dockerignore",
		"go.mod",
		"go.sum",
		"api/sandboxgen",
		"gormdb",
		"harness",
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
			name:         "worker-agent",
			baseImage:    "discobox-worker-agent:local",
			devPrefix:    "discobox-worker-agent:dev-",
			envImageKey:  "DISCOBOX_DOCKER_WORKER_IMAGE",
			envDigestKey: "DISCOBOX_DOCKER_WORKER_IMAGE_DIGEST",
			buildDir:     repoRoot,
			buildArgs:    []string{"build", "-f", "worker-agent/Dockerfile", "-t", "discobox-worker-agent:local", "."},
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
			files:        sortedFiles(seen),
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

func harnessMetadata(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var image struct {
		Harness json.RawMessage `json:"harness"`
	}
	if err := json.Unmarshal(data, &image); err != nil {
		return "", fmt.Errorf("read harness metadata from %s: %w", path, err)
	}
	if len(image.Harness) == 0 {
		return "", fmt.Errorf("read harness metadata from %s: missing harness object", path)
	}
	compact := bytes.Buffer{}
	if err := json.Compact(&compact, image.Harness); err != nil {
		return "", err
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

func workerAgentFiles(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", "./cmd/discobox-worker-agent")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list worker-agent deps: %w", err)
	}
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		dir := strings.TrimSpace(scanner.Text())
		if dir == "" || !inside(root, dir) {
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
	if changedSpecs == nil {
		changedSpecs = allSpecs
	}
	values := map[string]string{}
	for _, spec := range changedSpecs {
		image, imageID, err := buildImage(ctx, spec)
		if err != nil {
			return err
		}
		if spec.envImageKey != "" {
			values[spec.envImageKey] = image
		}
		if spec.envDigestKey != "" {
			values[spec.envDigestKey] = imageID
		}
	}
	return updateEnv(filepath.Join(repoRoot, envFile), values)
}

func buildImage(ctx context.Context, spec imageSpec) (string, string, error) {
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
	shortID := strings.TrimPrefix(imageID, "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	image := spec.devPrefix + shortID
	if err := runCommand(ctx, spec.buildDir, "docker", "tag", spec.baseImage, image); err != nil {
		return "", "", err
	}
	log.Printf("built %s as %s (%s)", spec.name, image, imageID)
	return image, imageID, nil
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
