// Package boot implements the sandbox-agent PID-1 init flow: it sets up the
// sandbox user, wires the image-declared data/cache volumes and manifest
// sources from the primary volumes the worker mounted, binds the config volume
// onto /etc/discobox, and then execs the container's real init (systemd). See
// ADR 0007.
package boot

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/harness"
)

const (
	// The worker mounts these primary volumes at fixed paths.
	dataMountPath    = "/.discobox/data"
	cacheMountPath   = "/.discobox/cache"
	configMountPath  = "/.discobox/config"
	sourcesMountPath = "/.discobox/sources"
	secretsMountPath = "/.discobox/secrets" //nolint:gosec // Filesystem path, not a credential.

	// etcDiscobox is where the config volume is rebound so the running
	// sandbox-agent and proxy find their material at the documented path.
	etcDiscobox = "/etc/discobox"

	// runSecrets is where the secrets volume is rebound so the running
	// sandbox-agent finds it at the documented path. The rebind happens after
	// systemd (PID 1) has already mounted its own tmpfs over /run — a Docker
	// bind mounted directly at /run/discobox/secrets would otherwise be
	// shadowed by that early-boot tmpfs mount.
	runSecrets = "/run/discobox/secrets" //nolint:gosec // Filesystem path, not a credential.

	// manifestName is the sandbox manifest file inside the config volume.
	manifestName = "sandbox.json"
)

// backingMount returns the primary volume that backs a declared volume kind.
func backingMount(kind harness.VolumeKind) string {
	if kind == harness.VolumeCache {
		return cacheMountPath
	}
	return dataMountPath
}

// volumeDir is the directory on the backing primary volume that stores a
// declared path's contents: /.discobox/{data|cache}/<target>.
func volumeDir(kind harness.VolumeKind, target string) string {
	return filepath.Join(backingMount(kind), strings.TrimPrefix(filepath.Clean(target), "/"))
}

// overlayDirs returns the upperdir and workdir used when a declared path is
// wired as an overlay (its target already ships content in the image).
func overlayDirs(volDir string) (upper, work string) {
	return filepath.Join(volDir, "upper"), filepath.Join(volDir, "work")
}

// useOverlay reports whether a declared path should be wired as an overlay
// rather than a plain bind. Overlay preserves image-shipped content as the
// lower layer while persisting writes to the volume, but is only safe for
// per-sandbox data volumes: a cache volume is shared across concurrently
// running sandboxes, and overlayfs upper/work dirs cannot be shared (ADR 0007).
func useOverlay(kind harness.VolumeKind, targetNonEmpty bool) bool {
	return kind == harness.VolumeData && targetNonEmpty
}

// sortVolumesByDepth orders volumes so that a parent path is always wired
// before a nested child (e.g. /var/lib/discobox before /var/lib/discobox/pnpm),
// so the child mounts onto the already-mounted parent rather than being shadowed.
func sortVolumesByDepth(volumes []harness.ResolvedVolume) {
	sort.SliceStable(volumes, func(i, j int) bool {
		return pathDepth(volumes[i].Path) < pathDepth(volumes[j].Path)
	})
}

func pathDepth(p string) int {
	return strings.Count(filepath.Clean(p), "/")
}
