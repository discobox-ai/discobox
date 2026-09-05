package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
)

// BuildModeEnv forces development image build-mode on or off, overriding the
// per-platform default.
const buildModeEnv = "DISCOBOX_DEV_DOCKER_IMAGE_BUILD"

// buildModeEnabled reports whether the watcher should describe how to build the
// development images instead of building them itself.
//
// Windows and macOS have no native Docker daemon to build on — the daemon lives
// inside the pool's VM — so there the watcher stamps a manifest and the server
// builds each image on the destination daemon through its embedded BuildKit.
// Linux keeps the copy-mode flow, where the host daemon builds once and the
// image is copied to each destination.
func buildModeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(buildModeEnv))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return runtime.GOOS != "linux"
}

// stampBuildModeImages writes the development image manifest and .env entries
// without building anything locally. Each image reference is content-addressed
// over that image's watched inputs, so an unchanged tree keeps the same
// reference and the server rebuilds nothing, while any input change produces a
// new reference that the server converges by building it.
func stampBuildModeImages(repoRoot string, specs []imageSpec) error {
	tags := make(map[string]string, len(specs))
	for _, spec := range specs {
		tag, err := contentImageTag(spec)
		if err != nil {
			return err
		}
		tags[spec.name] = tag
	}

	images := make([]devimage.Image, 0, len(specs))
	values := map[string]string{}
	for _, spec := range specs {
		// Intermediates are described too, unlike in copy-mode: the destination
		// daemon builds every image itself, so it needs the base before it can
		// build anything layered on it. It just has no .env key to publish.
		if spec.contextDir == "" || spec.dockerfile == "" {
			return fmt.Errorf("development image %s has no build context configured", spec.name)
		}
		args, err := buildModeArgs(spec, tags)
		if err != nil {
			return err
		}
		images = append(images, devimage.Image{
			Reference: tags[spec.name],
			Build: &devimage.BuildSpec{
				Dockerfile: filepath.ToSlash(spec.dockerfile),
				Context:    spec.contextDir,
				Args:       args,
			},
		})
		if spec.envImageKey != "" {
			values[spec.envImageKey] = tags[spec.name]
		}
		log.Printf("stamped %s as %s (built on demand by the server)", spec.name, tags[spec.name])
	}

	manifest, err := devimage.NewManifest(images)
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

// buildModeArgs renders a spec's build arguments, resolving a dependency on the
// image it builds FROM to that image's stamped reference so the server can order
// the builds.
func buildModeArgs(spec imageSpec, tags map[string]string) (map[string]string, error) {
	args := map[string]string{}
	if spec.metadataFile != "" {
		metadata, err := harnessMetadata(spec.metadataFile)
		if err != nil {
			return nil, err
		}
		args[spec.metadataArg] = metadata
	}
	if spec.parentArg != "" {
		parentTag, ok := tags[spec.parent]
		if !ok {
			return nil, fmt.Errorf("no %s image stamped for %s", spec.parent, spec.name)
		}
		args[spec.parentArg] = parentTag
	}
	if len(args) == 0 {
		return nil, nil
	}
	return args, nil
}

// contentImageTag derives a stable image reference from the contents of a
// spec's watched inputs. Copy-mode addresses images by the built image ID,
// which build-mode cannot know in advance because nothing is built here.
func contentImageTag(spec imageSpec) (string, error) {
	digest := sha256.New()
	for _, path := range spec.files {
		rel, err := filepath.Rel(spec.buildDir, path)
		if err != nil {
			rel = path
		}
		fmt.Fprintf(digest, "%s\n", filepath.ToSlash(rel))
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("hash development image input %s: %w", path, err)
		}
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash development image input %s: %w", path, copyErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	// A child embeds its parent's reference, but that is resolved after tagging;
	// the parent's own inputs are already part of every child spec's file set, so
	// a parent change still changes the child's tag.
	return spec.devPrefix + hex.EncodeToString(digest.Sum(nil))[:12], nil
}
