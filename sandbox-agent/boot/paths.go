// Package boot implements the sandbox-agent PID-1 init flow: it sets up the
// sandbox user, wires the image-declared data/cache volumes and manifest
// sources from the primary volumes the pool agent mounted, binds the config volume
// onto /etc/discobox, and then execs the container's real init (systemd). See
// ADR 0007.
package boot

import (
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/harness"
)

const (
	// The pool agent mounts these primary volumes at fixed paths.
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

// cacheUsersDir is the level at which the pool cache is partitioned by user,
// below which each partition mirrors target paths exactly as the data volume
// does. It is dot-prefixed because its siblings are those mirrored paths -- both
// the shared cache paths, which stay at the cache root, and everything a
// pre-partition agent wrote there. No image declares a cache path at "/.users",
// so a target cannot be mistaken for the partition or shadow it.
const cacheUsersDir = ".users"

// volumeDir is the directory on the backing primary volume that stores a
// declared path's contents:
//
//	/.discobox/data/<target>                  a per-sandbox data volume
//	/.discobox/cache/.users/<uid>/<target>     a user-scoped cache path
//	/.discobox/cache/<target>                  a shared cache path
//
// A data volume is this sandbox's alone, so it has nobody to be partitioned
// from. A cache path is shared by every sandbox the pool runs, and a user-scoped
// one is chowned to the sandbox user and filled by it -- so two users on one
// directory leave each other files they cannot write, and re-chown the
// mountpoints out from under each other on every boot. Two clients of one server
// are two users whenever their local accounts differ, which is ordinary rather
// than exotic (ADR 0094, cache partition).
//
// The uid is the whole key. A name is not a uid, a gid does not decide who may
// write a file, and a home directory is where files go rather than whose they
// are; two sandboxes that agree on a uid can share, which is what a pool-shared
// cache is for.
//
// A shared cache path keeps the unpartitioned location deliberately. It is the
// path such a tree already lives at, so declaring the scope moves nothing, costs
// no re-seed, and strands no copy -- and an older sandbox agent, which knows
// nothing of partitions, lands on the same directory and goes on sharing it.
//
// These are guest paths, so this file joins with "path" rather than
// "path/filepath": the sandbox is Linux whatever host the tests run on, and
// filepath would splice a backslash in on a Windows runner.
func volumeDir(v harness.ResolvedVolume, uid int) string {
	trimmed := strings.TrimPrefix(path.Clean(v.Path), "/")
	if v.Kind != harness.VolumeCache {
		return path.Join(dataMountPath, trimmed)
	}
	if v.Scope == harness.VolumeScopeShared {
		return path.Join(cacheMountPath, trimmed)
	}
	return path.Join(cacheMountPath, cacheUsersDir, strconv.Itoa(uid), trimmed)
}

// overlayDirs returns the upperdir and workdir used when a declared path is
// wired as an overlay (its target already ships content in the image).
func overlayDirs(volDir string) (upper, work string) {
	return path.Join(volDir, "upper"), path.Join(volDir, "work")
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
	return strings.Count(path.Clean(p), "/")
}
