// Package devimage defines the development Docker image manifest shared by the
// image watcher and the server.
package devimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// SyncEnv enables development image synchronization to every Docker daemon
	// used by a pool provider.
	SyncEnv = "DISCOBOX_DEV_DOCKER_IMAGE_SYNC"
	// ManifestEnv names the watcher-produced development image manifest.
	ManifestEnv = "DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST"

	// ManifestVersion is the current development image manifest format.
	ManifestVersion = 1
)

// Image identifies one watcher-built image by the reference used by Discobox
// and the immutable Docker image configuration ID expected at that reference.
//
// In copy-mode (the default) ID is required: the image is built on a source
// Docker daemon and copied to each destination, and ID content-addresses it. In
// build-mode Build is set instead: the host has no Docker daemon to build on
// (Windows/macOS), so each destination builds the image itself from Build, and
// a unique per-build Reference (e.g. a dev-<timestamp> tag) is the freshness
// key, making ID unnecessary.
type Image struct {
	Reference string     `json:"reference"`
	ID        string     `json:"id,omitempty"`
	Build     *BuildSpec `json:"build,omitempty"`
}

// BuildSpec describes how to build a development image on the destination
// Docker daemon (via BuildKit) instead of copying it from a source daemon.
type BuildSpec struct {
	// Dockerfile is the path to the Dockerfile, relative to Context.
	Dockerfile string `json:"dockerfile"`
	// Context is the absolute build-context root on the host.
	Context string `json:"context"`
	// Args are --build-arg values. Cross-image dependencies are expressed by
	// setting an arg value to another manifest image's Reference (for example a
	// harness image's SANDBOX_AGENT_IMAGE), which orders the builds.
	Args map[string]string `json:"args,omitempty"`
	// Target optionally selects a build stage.
	Target string `json:"target,omitempty"`
	// Platform is the target platform, for example "linux/amd64".
	Platform string `json:"platform,omitempty"`
}

// Manifest is the complete watcher-built image set to converge onto a Docker
// daemon before that daemon hosts a development pool.
type Manifest struct {
	Version int     `json:"version"`
	Images  []Image `json:"images"`
}

// NewManifest validates and canonicalizes a development image set.
func NewManifest(images []Image) (Manifest, error) {
	manifest := Manifest{
		Version: ManifestVersion,
		Images:  append([]Image(nil), images...),
	}
	for i := range manifest.Images {
		manifest.Images[i].Reference = strings.TrimSpace(manifest.Images[i].Reference)
		manifest.Images[i].ID = strings.TrimSpace(manifest.Images[i].ID)
	}
	sort.Slice(manifest.Images, func(i, j int) bool {
		return manifest.Images[i].Reference < manifest.Images[j].Reference
	})
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate verifies that a manifest is complete and unambiguous.
func (m Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("development image manifest version %d is unsupported; want %d", m.Version, ManifestVersion)
	}
	if len(m.Images) == 0 {
		return errors.New("development image manifest has no images")
	}
	seen := make(map[string]struct{}, len(m.Images))
	for i, image := range m.Images {
		if strings.TrimSpace(image.Reference) == "" {
			return fmt.Errorf("development image manifest image %d has no reference", i)
		}
		if image.Build != nil {
			if strings.TrimSpace(image.Build.Context) == "" {
				return fmt.Errorf("development image manifest image %q build has no context", image.Reference)
			}
			if strings.TrimSpace(image.Build.Dockerfile) == "" {
				return fmt.Errorf("development image manifest image %q build has no dockerfile", image.Reference)
			}
		} else {
			if strings.TrimSpace(image.ID) == "" {
				return fmt.Errorf("development image manifest image %q has no ID", image.Reference)
			}
			if !strings.HasPrefix(image.ID, "sha256:") {
				return fmt.Errorf("development image manifest image %q has invalid ID %q", image.Reference, image.ID)
			}
		}
		if _, ok := seen[image.Reference]; ok {
			return fmt.Errorf("development image manifest repeats reference %q", image.Reference)
		}
		seen[image.Reference] = struct{}{}
	}
	return nil
}

// Read loads and validates a watcher-produced manifest.
func Read(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read development image manifest %s: %w", path, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode development image manifest %s: %w", path, err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate development image manifest %s: %w", path, err)
	}
	return NewManifest(manifest.Images)
}

// WriteAtomic installs a validated manifest without exposing a partial file to
// an Air-restarted server.
func WriteAtomic(path string, manifest Manifest) error {
	canonical, err := NewManifest(manifest.Images)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create development image manifest directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	installed := false
	defer func() {
		_ = tmp.Close()
		if !installed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	installed = true
	return nil
}
