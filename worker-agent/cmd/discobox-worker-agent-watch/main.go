package main

import (
	"bufio"
	"bytes"
	"context"
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

const (
	baseImage = "discobox-worker-agent:local"
	envFile   = ".env"
)

var fixedWatchFiles = []string{
	"Dockerfile",
	"go.mod",
	"go.sum",
	"../.dockerignore",
	"../go.mod",
	"../go.sum",
	"../id/id.go",
}

func main() {
	log.SetFlags(0)
	ctx := context.Background()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	workerAgentRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	repoRoot := filepath.Dir(workerAgentRoot)
	if _, err := os.Stat(filepath.Join(repoRoot, "worker-agent", "go.mod")); err != nil {
		repoRoot = workerAgentRoot
	}
	files, err := workerAgentFiles(ctx, workerAgentRoot)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(files)+len(fixedWatchFiles))
	for _, file := range files {
		seen[file] = struct{}{}
	}
	for _, file := range fixedWatchFiles {
		addFile(workerAgentRoot, file, seen)
	}
	files = sortedFiles(seen)
	if len(files) == 0 {
		return errors.New("no worker-agent files discovered")
	}
	log.Printf("watching %d worker-agent Docker inputs", len(files))
	state := snapshot(files)
	if err := buildAndWriteEnv(ctx, repoRoot, workerAgentRoot); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			next := snapshot(files)
			if !changed(state, next) {
				continue
			}
			state = next
			log.Print("worker-agent Docker input changed; rebuilding image")
			if err := buildAndWriteEnv(ctx, repoRoot, workerAgentRoot); err != nil {
				log.Printf("build failed: %v", err)
			}
		}
	}
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

func buildAndWriteEnv(ctx context.Context, repoRoot, _ string) error {
	if err := runCommand(ctx, repoRoot, "docker", "build", "-f", "worker-agent/Dockerfile", "-t", baseImage, "."); err != nil {
		return err
	}
	imageID, err := commandOutput(ctx, repoRoot, "docker", "image", "inspect", "-f", "{{.Id}}", baseImage)
	if err != nil {
		return err
	}
	imageID = strings.TrimSpace(imageID)
	shortID := strings.TrimPrefix(imageID, "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	image := "discobox-worker-agent:dev-" + shortID
	if err := runCommand(ctx, repoRoot, "docker", "tag", baseImage, image); err != nil {
		return err
	}
	if err := updateEnv(filepath.Join(repoRoot, envFile), map[string]string{
		"DISCOBOX_DOCKER_WORKER_IMAGE":        image,
		"DISCOBOX_DOCKER_WORKER_IMAGE_DIGEST": imageID,
	}); err != nil {
		return err
	}
	log.Printf("updated %s with %s (%s)", envFile, image, imageID)
	return nil
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
