package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/client"
	"github.com/obot-platform/discobox/layout"

	"github.com/obot-platform/discobox/pool-agent/proxyagent"
)

// Archiving keeps a sandbox's durable tree and drops everything else (ADR 0022
// §6). What counts as durable is decided by the storage split in layout: the
// per-sandbox data/config/secrets/sources subtrees are kept, the container and
// its proxy material go, and the pool cache is untouched because it is shared
// by the whole pool and was never this sandbox's to release.
//
// The marker file below is what makes retained data legible as retained. On
// disk an archived sandbox and a sandbox whose container was lost out of band
// look identical — a directory with no container — and they must not be treated
// the same: one is holding data by intent, the other is garbage awaiting the
// reaper's retention window. The marker is the only thing that tells them
// apart, so it is what the reaper skips and what refuses an on-demand start.
const sandboxArchiveMarker = ".discobox-archived"

// ErrArchived reports that a sandbox exists as data but has no runtime. It is
// returned instead of starting one, so that using an archived sandbox fails
// with something the caller can act on rather than silently undoing the archive
// (ADR 0022 §5). Both error mappers in the server package translate it to 409.
var ErrArchived = errors.New("sandbox is archived; unarchive it to use it")

func sandboxArchiveMarkerPath(root string) string {
	return filepath.Join(root, sandboxArchiveMarker)
}

// sandboxIsArchived reports whether a sandbox tree is being retained by intent.
func sandboxIsArchived(root string) bool {
	_, err := os.Stat(sandboxArchiveMarkerPath(root))
	return err == nil
}

func (r *DockerSandboxRuntime) sandboxRoot(sandboxID string) string {
	return resolve(layout.Sandbox(r.projectID, r.poolID, sandboxID))
}

// SandboxIsArchived reports whether this pool holds the given sandbox as
// archived data.
func (r *DockerSandboxRuntime) SandboxIsArchived(sandboxID string) bool {
	return sandboxIsArchived(r.sandboxRoot(sandboxID))
}

// ArchiveSandbox tears the sandbox's runtime down and keeps its data.
//
// It takes the same per-sandbox power lock as start and stop, which is what
// makes it safe against the on-demand start latch: an auto-start already in
// flight completes or waits, and cannot interleave half way through the
// teardown. Once the marker is written, subsequent starts are refused.
//
// The operation is idempotent. A sandbox with no container archives fine —
// that is the state a second archive, or an archive after an out-of-band
// removal, arrives in — because what makes a sandbox archived is the marker,
// not the absence of a container.
func (r *DockerSandboxRuntime) ArchiveSandbox(ctx context.Context, sandboxID string) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()

	root := r.sandboxRoot(sandboxID)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			// Nothing to retain. Refusing here would make archive fail for a
			// sandbox the control plane still has a row for, with no way
			// forward except purge; marking it archived would claim data that
			// does not exist.
			return fmt.Errorf("archive sandbox %s: %w", sandboxID, ErrNotFound)
		}
		return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
	}

	// Mark before tearing down. If the agent dies mid-archive, a marked tree
	// with a surviving container is recoverable — the next archive finishes the
	// job, and nothing starts it in the meantime. The reverse order would leave
	// an unmarked, container-less tree, which the reaper reads as garbage.
	if err := writeSandboxArchiveMarker(root, time.Now()); err != nil {
		return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
	}

	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil {
		r.PublishSandboxState(ctx, sandboxID, StateStopping)
		if _, err := r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return err
		}
	}
	// The proxy material is disposable: an unarchive recreates the container,
	// and creation stages fresh material and a fresh client certificate for it.
	if err := proxyagent.RemoveSandboxSentinels(r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	if err := proxyagent.RemoveSandboxMaterial(r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	// A tombstone from an earlier period without a container would otherwise
	// outlive the archive and start the reaper's clock the moment the sandbox is
	// unarchived and stopped again.
	_ = os.Remove(filepath.Join(root, sandboxVolumeTombstone))
	return nil
}

// clearSandboxArchiveMarker un-archives a tree. Creation calls it, which is the
// whole of what unarchive needs on this side: the existing reuse-the-tree path
// already restores the sandbox against the data that was kept.
func clearSandboxArchiveMarker(root string) error {
	if err := os.Remove(sandboxArchiveMarkerPath(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeSandboxArchiveMarker(root string, at time.Time) error {
	return os.WriteFile(sandboxArchiveMarkerPath(root), []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o600)
}
