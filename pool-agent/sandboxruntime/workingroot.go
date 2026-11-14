package sandboxruntime

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

// workingRootStashName, at the root of a sandbox's tree, holds what a removed
// container of it had at the working root in its own writable layer, as the
// archive Docker made of it, until the next container built for the sandbox
// is given it (ADR 26-11-14-302 §3).
//
// A sandbox agent from before that ADR left a box with no source working in
// its container's own /workspace, which removing the container deletes. Every
// path that removes a sandbox's container to build another stashes it first
// (stashWorkingRoot), and the next container has it copied into its layer
// before it starts (restoreWorkingRoot), where boot moves it onto the data
// root. A container whose agent moved its working root onto the data root has
// nothing left at it in its layer, so a box is carried over once.
//
// Beside the sandbox's volumes, as runtime-config.json is, never in them: the
// box can write its volumes, and a name there it replaced with a link or a
// FIFO would be followed by this agent as root on the host (pool-agent
// REVIEW.md, "Nothing is written into a sandbox after create"). It survives
// an archive and leaves with a delete. It does not travel with an export
// (ADR 0129), which carries the volumes only.
const workingRootStashName = "working-root-carried.tar"

func (r *sandboxCore) workingRootStashPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), workingRootStashName)
}

// stashWorkingRoot keeps what containerID, a container of sandboxID about to be
// removed, has at the working root in its own layer (workingRootStashName).
//
// Read through Docker's archive API, which reads the container's filesystem
// from the daemon's side, so what boot bound over the working root inside the
// container is not in it: only the layer is. Nothing at the working root but
// the directory itself is nothing to keep. A stash already there is kept as it
// is: it holds what no container has been given yet, and this container was
// built from it. The archive is stored, never extracted on this host: its
// contents are the box's.
//
// A container with a Docker mount at or above the working root has nothing of
// its own there to keep: the mount would be in the archive, and carried into
// every container after it.
//
// The copy is the box's work, and the removal waits on it: one that fails
// leaves the container in place and fails the removal, which a retry runs
// again, rather than removing the work with it. It is cut loose from its
// caller's cancellation, so a client that stops waiting does not throw a
// large copy away part of the way through, and bounded on its own
// (workingRootStashTimeout).
func (r *DockerSandboxRuntime) stashWorkingRoot(ctx context.Context, sandboxID, containerID string) error {
	stash := r.workingRootStashPath(sandboxID)
	if _, err := os.Lstat(stash); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workingRootStashTimeout)
	defer cancel()
	inspect, err := r.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox %s's container for its working root: %w", sandboxID, err)
	}
	root := r.paths.WorkingRoot()
	for _, m := range inspect.Container.Mounts {
		if pathWithin(m.Destination, root) {
			return nil
		}
	}
	copied, err := r.client.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: r.paths.WorkingRoot()})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sandbox %s's working root from its container: %w", sandboxID, err)
	}
	defer copied.Content.Close()
	if err := os.MkdirAll(filepath.Dir(stash), 0o755); err != nil {
		return err
	}
	f, err := fsdurable.Create(stash, 0o600)
	if err != nil {
		return err
	}
	defer f.Discard() //nolint:errcheck // A no-op once committed; the error that matters is returned below.
	entries, err := countArchiveEntries(io.TeeReader(copied.Content, f))
	if err != nil {
		return fmt.Errorf("read sandbox %s's working root from its container: %w", sandboxID, err)
	}
	if entries <= 1 {
		return nil
	}
	if err := f.Commit(); err != nil {
		return err
	}
	return nil
}

// workingRootStashTimeout bounds a stash: a copy of a box's whole working
// root, which can be large, through this agent onto the pool host's disk.
const workingRootStashTimeout = 30 * time.Minute

// pathWithin reports whether p is dir or lies below it, as guest paths.
func pathWithin(dir, p string) bool {
	dir, p = path.Clean(dir), path.Clean(p)
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// countArchiveEntries reads the whole of a tar stream and says how many
// entries it holds.
func countArchiveEntries(r io.Reader) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		n++
	}
	// The end-of-archive blocks and any padding after them, so the copy
	// beside the reader holds the archive Docker sent whole.
	_, err := io.Copy(io.Discard, r)
	return n, err
}

// restoreWorkingRoot gives containerID, a container just created for sandboxID
// and not yet started, what its sandbox's removed container had at the working
// root (stashWorkingRoot), in its own layer, for boot to move onto the data
// root. Docker extracts it, in the container's filesystem, with the owners it
// had there. The stash is removed once it is given, before the container
// starts, so no boot moves the same entries in twice.
func (r *DockerSandboxRuntime) restoreWorkingRoot(ctx context.Context, sandboxID, containerID string) error {
	stash := r.workingRootStashPath(sandboxID)
	f, err := os.Open(stash) //nolint:gosec // G304: this agent's own file in the sandbox's tree
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	// The archive's entries are named from the working root's own name, so
	// it is extracted into the directory that holds it.
	parent := path.Dir(r.paths.WorkingRoot())
	if _, err := r.client.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: parent, Content: f, CopyUIDGID: true}); err != nil {
		return fmt.Errorf("give sandbox %s's new container the working root its last one had: %w", sandboxID, err)
	}
	// Docker wrote it, so there is no file of ours to sync (placeAgent).
	fsdurable.SyncAll()
	return fsdurable.Remove(stash)
}
