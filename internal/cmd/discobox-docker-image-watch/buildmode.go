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

	"github.com/discobox-ai/discobox/pkg/devimage"
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
// without building anything locally. Each image or bundle reference is
// content-addressed over its watched inputs, so an unchanged tree keeps the
// same reference and the server rebuilds nothing, while any input change
// produces a new reference that the server converges by building it. A bundle
// is recorded among the manifest's bundles: a pool's daemon builds it, and the
// server saves it as the layout it serves pools from (ADR 26-10-16-423 §13).
func stampBuildModeImages(repoRoot string, specs []imageSpec) error {
	images := make([]devimage.Image, 0, len(specs))
	var bundles []devimage.Bundle
	values := map[string]string{}
	for _, spec := range specs {
		// Every spec is described, including one with no .env key to publish:
		// the destination daemon builds every image and bundle itself.
		if spec.contextDir == "" || spec.dockerfile == "" {
			return fmt.Errorf("development image %s has no build context configured", spec.name)
		}
		tag, err := contentImageTag(spec)
		if err != nil {
			return err
		}
		args, err := buildModeArgs(spec)
		if err != nil {
			return err
		}
		build := &devimage.BuildSpec{
			Dockerfile: filepath.ToSlash(spec.dockerfile),
			Context:    spec.contextDir,
			Target:     spec.target,
			Args:       args,
		}
		if spec.bundle {
			bundles = append(bundles, devimage.Bundle{Reference: tag, Build: build})
		} else {
			images = append(images, devimage.Image{Reference: tag, Build: build})
		}
		if spec.envImageKey != "" {
			values[spec.envImageKey] = tag
		}
		log.Printf("stamped %s as %s (built on demand by the server)", spec.name, tag)
	}

	manifest, err := devimage.NewManifest(images, bundles)
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

// buildModeArgs renders a spec's build arguments: the manifest metadata read
// from image.json, or none for an image whose build labels no manifest.
func buildModeArgs(spec imageSpec) (map[string]string, error) {
	args := map[string]string{}
	if spec.metadataFile != "" {
		metadata, err := harnessMetadata(spec.metadataFile)
		if err != nil {
			return nil, err
		}
		args[spec.metadataArg] = metadata
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
	return spec.devPrefix + hex.EncodeToString(digest.Sum(nil))[:12], nil
}
