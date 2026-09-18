package sandboxruntime

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxtree"
	"github.com/discobox-ai/discobox/sandboxuser"
	"github.com/discobox-ai/discobox/tarsums"
)

// A sandbox's durable tree is the half of it that outlives its container: the
// subtrees a create reuses rather than rebuilds (ADR 0022 §6). Exporting it and
// restoring it elsewhere is what makes a discobox portable (ADR 0123). What it
// holds, and how it is written, is sandboxtree's.
//
// The two halves are not read by the same party. `data` and `sources` are the
// sandbox's, and only the sandbox can say which of its declared paths stay
// behind and where they live, so the sandbox agent's export mode reads them,
// from the sandbox's own image (ADR 0129 §1). `origins` is this pool's, and
// this agent adds it.

// ErrTreeExists refuses a restore onto a sandbox this pool already holds.
// Overwriting would merge two sandboxes' data into one tree, and there is no
// reading of "import" that means that.
var ErrTreeExists = errors.New("sandbox data already exists on this pool")

// ErrSandboxRunning refuses to read a tree out from under a running container.
// A tar of a live tree can catch a git index mid-write or a sqlite file between
// its journal and its pages, and the reader finds out only on the far side of a
// transfer (ADR 0123 §2).
var ErrSandboxRunning = errors.New("sandbox is running; stop it before exporting it")

// ErrExportUnsupported refuses to export a sandbox whose image carries no
// export mode: its sandbox agent predates it, and would read the argument as
// an ordinary start (ADR 0129 §3).
var ErrExportUnsupported = errors.New("the sandbox's image predates export; run `discobox admin box upgrade` on it, then export it")

// ErrExportInProgress refuses a second export of a sandbox while one is
// running: killing the first would cost whoever is reading it.
var ErrExportInProgress = errors.New("this sandbox is already being exported")

// TreeImage is the image a sandbox's tree is read with: the one it is pinned
// to, named the way a create names it. The control plane supplies it because
// it owns the pin, and because an archived sandbox, or one whose create failed,
// has no container here to read it from.
type TreeImage struct {
	Name   string
	Digest string
}

// exportAgentPath is the sandbox agent inside every sandbox image, which the
// export container runs in place of the image's init.
const exportAgentPath = "/usr/local/bin/discobox-sandbox-agent"

// exportStderrLimit bounds what is kept of the export mode's own account of
// itself: enough for the error that ended it, and not a log.
const exportStderrLimit = 16 * 1024

// ExportTree streams the sandbox's durable tree as a tar archive.
//
// The stream is produced while the caller reads, because the tree is
// gigabytes of workspace and nothing here should hold it. Everything that can
// be refused -- a running sandbox, an image without the export mode, a mode
// that fails before it writes a byte -- is refused before this returns, so it
// is a status. A failure after that reaches the caller as a read error on a
// body that has already begun, which is the same bargain every streaming route
// in this repository makes, and it leaves the archive without its SHA256SUMS,
// so a reader that never saw the error still refuses what it got.
func (r *DockerSandboxRuntime) ExportTree(ctx context.Context, sandboxID string, image TreeImage) (io.ReadCloser, error) {
	root := r.sandboxRoot(sandboxID)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, ErrNotFound)
		}
		return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, err)
	}
	// Asked before anything starts, so "it is running" is a status and not a
	// truncated archive. It is not a lock: a start that races this loses the
	// check, which is why stopping first is the caller's job and not a promise
	// made here.
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil && sb.Status == StatusRunning {
		return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, ErrSandboxRunning)
	}
	exported, err := r.runExportMode(ctx, sandboxID, image)
	if err != nil {
		return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, err)
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := composeTree(ctx, writer, exported, filepath.Join(root, sandboxtree.Origins))
		// Closing what the sandbox is still writing ends its container: its next
		// write fails, the copy returns, and the container is removed.
		_ = exported.Close()
		// CloseWithError(nil) is Close, so one call covers both outcomes and the
		// reader sees the failure rather than a clean end of archive.
		_ = writer.CloseWithError(err)
	}()
	return &exportStream{Reader: reader, closer: reader, done: done}, nil
}

// composeTree writes the tree this pool serves: the subtrees the sandbox
// exported, verified and re-emitted, then this pool's own origins, then
// SHA256SUMS.
//
// The sandbox's stream is untrusted input. Its SHA256SUMS is checked as it is
// read, and it may name nothing outside `data` and `sources` -- an `origins`
// entry from the sandbox would stand in for the pool's own. The archive's
// SHA256SUMS is written by the Close at the end and nowhere else, so every early
// return leaves an archive no reader accepts.
func composeTree(ctx context.Context, w io.Writer, exported io.Reader, origins string) error {
	archive := tarsums.NewWriter(w)
	if err := sandboxtree.Copy(archive, tarsums.NewReader(exported), sandboxtree.Data, sandboxtree.Sources); err != nil {
		return fmt.Errorf("read the tree the sandbox exported: %w", err)
	}
	if _, err := os.Lstat(origins); err == nil {
		if err := sandboxtree.NewWriter(archive).AddDir(ctx, origins, sandboxtree.Origins, nil); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		// A sandbox with no push-delivered source has no origins directory.
		// Absent is not empty and not an error: what is there is what travels.
		return err
	}
	return archive.Close()
}

// runExportMode reads the sandbox's `data` and `sources` by running its own
// image in export mode, and returns the archive the mode writes.
//
// The container sees exactly the trees it is to read, read-only, and its
// config for the declarations and user it resolves; nothing else. No network,
// no capability but reading past permissions, a read-only root. It runs as PID
// 1 and starts nothing, so the reads happen in a namespace holding only this
// sandbox's own trees, not on this host as root -- a symlink the sandbox wrote
// resolves inside the sandbox's world, where it always pointed.
func (r *DockerSandboxRuntime) runExportMode(ctx context.Context, sandboxID string, image TreeImage) (io.ReadCloser, error) {
	imageID, err := r.exportImage(ctx, sandboxID, image)
	if err != nil {
		return nil, err
	}
	var mounts []mount.Mount
	var subtrees []string
	for _, tree := range []struct {
		subtree, host, target string
	}{
		{sandboxtree.Data, r.sandboxDataRootPath(sandboxID), sandboxDataMount},
		{sandboxtree.Sources, r.sandboxSourcesRoot(sandboxID), sandboxSourcesMount},
		{"", r.sandboxConfigRoot(sandboxID), sandboxConfigMount},
	} {
		if _, err := os.Stat(tree.host); err != nil {
			if os.IsNotExist(err) {
				// A sandbox whose create stopped early may have none of these.
				// What is there is what travels; an absent config means no
				// declarations, so its data travels whole.
				continue
			}
			return nil, err
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: r.daemonPath(tree.host), Target: tree.target, ReadOnly: true})
		if tree.subtree != "" {
			subtrees = append(subtrees, tree.subtree)
		}
	}
	user, err := r.recordedSandboxUser(sandboxID)
	if err != nil {
		return nil, err
	}
	name := sandboxContainerName(r.poolID, sandboxID) + "-export"
	if err := r.removeFinishedExport(ctx, name); err != nil {
		return nil, err
	}
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      imageID,
			Entrypoint: []string{exportAgentPath, "export"},
			Cmd:        subtrees,
			// The same user the sandbox boots as, so %HOME% resolves where boot
			// put it (ADR 0129 §1).
			Env:             envList(envWithSandboxUser(nil, user)),
			User:            "0:0",
			Labels:          r.exportLabels(sandboxID),
			AttachStdout:    true,
			AttachStderr:    true,
			NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			Mounts:         mounts,
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"DAC_READ_SEARCH"},
			SecurityOpt:    []string{"no-new-privileges"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create export container: %w", err)
	}
	remove := func() {
		// Not the request's context: the container has to go whether or not
		// whoever asked for the export is still there.
		if _, err := r.client.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			slog.WarnContext(ctx, "could not remove an export container", "sandboxId", sandboxID, "container", created.ID, "error", err)
		}
	}
	// Attached before it starts, so not a byte of the archive is written before
	// something is reading it.
	attached, err := r.client.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		remove()
		return nil, fmt.Errorf("attach to export container: %w", err)
	}
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		attached.Close()
		remove()
		return nil, fmt.Errorf("start export container: %w", err)
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remove()
		defer attached.Close()
		stderr := &tailBuffer{limit: exportStderrLimit}
		_, copyErr := stdcopy.StdCopy(writer, stderr, attached.Reader)
		_ = writer.CloseWithError(exportModeResult(ctx, r.client, created.ID, copyErr, stderr))
	}()
	stream := &exportStream{closer: reader, done: done}
	// Wait for the archive's first byte before answering: a mode that fails at
	// once -- a manifest it cannot read, a user it cannot resolve -- exits
	// without writing one, and that is an error the caller can still be given
	// as a status.
	buffered := bufio.NewReader(reader)
	if _, err := buffered.Peek(1); err != nil {
		_ = stream.Close()
		return nil, err
	}
	stream.Reader = buffered
	return stream, nil
}

// exportImage resolves the image a sandbox's tree is read with, and refuses
// one without the export mode.
func (r *DockerSandboxRuntime) exportImage(ctx context.Context, sandboxID string, image TreeImage) (string, error) {
	reference, digest := strings.TrimSpace(image.Name), strings.TrimSpace(image.Digest)
	if reference == "" && digest == "" {
		return "", fmt.Errorf("%w: the export named no image to read the sandbox with", ErrImageUnavailable)
	}
	// The same resolution a create makes, so an export reads with the image the
	// sandbox runs and fails, when that image is gone, the way a start would.
	imageID, err := r.resolveSandboxImage(ctx, sandboxID, reference, digest)
	if err != nil {
		return "", err
	}
	inspected, err := r.client.ImageInspect(ctx, imageID)
	if err != nil {
		return "", fmt.Errorf("inspect image %q: %w", imageID, err)
	}
	if inspected.Config == nil || inspected.Config.Labels[harness.TreeExportLabel] != harness.TreeExportLabelValue {
		return "", ErrExportUnsupported
	}
	return imageID, nil
}

// exportModeResult is how the export mode ended: nil only when its output was
// copied whole and it exited 0. Its stderr is the reason when it did not.
func exportModeResult(ctx context.Context, docker client.APIClient, containerID string, copyErr error, stderr *tailBuffer) error {
	if copyErr != nil {
		return fmt.Errorf("read the export: %w", copyErr)
	}
	waited := docker.ContainerWait(context.WithoutCancel(ctx), containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case result := <-waited.Result:
		if result.StatusCode == 0 {
			return nil
		}
		return fmt.Errorf("the sandbox's export exited %d: %s", result.StatusCode, stderr.message())
	case err := <-waited.Error:
		return fmt.Errorf("wait for the export container: %w", err)
	}
}

// removeFinishedExport clears an export container left behind by an agent that
// stopped mid-export, and refuses to disturb one still running: two exports of
// one sandbox are one too many, and killing the first would cost whoever is
// reading it.
func (r *DockerSandboxRuntime) removeFinishedExport(ctx context.Context, name string) error {
	inspected, err := r.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if inspected.Container.State != nil && inspected.Container.State.Running {
		return ErrExportInProgress
	}
	_, err = r.client.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// exportLabels mark an export container as this pool's, and deliberately not
// as a sandbox: it carries no sandboxLabelManaged, so nothing that lists this
// pool's sandboxes ever mistakes one for the sandbox it is reading.
func (r *DockerSandboxRuntime) exportLabels(sandboxID string) map[string]string {
	return map[string]string{
		sandboxLabelProject: r.projectID,
		sandboxLabelPool:    r.poolID,
		sandboxLabelSandbox: sandboxID,
		exportLabel:         "true",
	}
}

// exportLabel marks a container as an export of a sandbox's tree.
const exportLabel = "io.discobox.export"

// recordedSandboxUser is the user this sandbox's sandbox.json names -- the one
// its boot resolved -- or nobody, for a sandbox whose create never wrote one.
func (r *DockerSandboxRuntime) recordedSandboxUser(sandboxID string) (sandboxuser.User, error) {
	data, err := os.ReadFile(filepath.Join(r.sandboxConfigRoot(sandboxID), sandboxDocumentName))
	if err != nil {
		if os.IsNotExist(err) {
			return sandboxuser.User{}, nil
		}
		return sandboxuser.User{}, err
	}
	var config sandboxconfig.Config
	if err := json.Unmarshal(data, &config); err != nil {
		return sandboxuser.User{}, fmt.Errorf("parse %s: %w", sandboxDocumentName, err)
	}
	return config.User, nil
}

// exportStream is a stream an export produces, whose Close also waits for
// what produces it to finish -- so an export, and the container behind it,
// never outlives whoever was reading it.
type exportStream struct {
	io.Reader
	closer io.Closer
	done   <-chan struct{}
}

func (e *exportStream) Close() error {
	err := e.closer.Close()
	<-e.done
	return err
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = b.data[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) message() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if text := strings.TrimSpace(string(b.data)); text != "" {
		return text
	}
	return "it gave no reason"
}

// ImportTree restores a durable tree for a sandbox this pool does not yet hold.
//
// It writes the tree and nothing else: no container, no proxy material, no
// marker. What it leaves behind is exactly the shape an archived sandbox has,
// which is what lets the ordinary create that follows adopt it (ADR 0123 §3).
//
// A restore that fails part way removes what it wrote. Half a tree is worse
// than none: it would be adopted by a create just as readily as a whole one,
// and the sandbox would come up with a workspace missing files nobody can name.
func (r *DockerSandboxRuntime) ImportTree(ctx context.Context, sandboxID string, tree io.Reader) (err error) {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()

	root := r.sandboxRoot(sandboxID)
	if _, statErr := os.Stat(root); statErr == nil {
		return fmt.Errorf("import sandbox %s: %w", sandboxID, ErrTreeExists)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("import sandbox %s: %w", sandboxID, statErr)
	}
	if mkErr := os.MkdirAll(root, 0o755); mkErr != nil {
		return fmt.Errorf("import sandbox %s: %w", sandboxID, mkErr)
	}
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(root); rmErr != nil {
				slog.ErrorContext(ctx, "could not remove a partially restored sandbox tree",
					"sandboxId", sandboxID, "path", root, "error", rmErr)
			}
		}
	}()
	if err := readTree(ctx, tree, root); err != nil {
		return fmt.Errorf("import sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// readTree restores a tar into the sandbox tree at rootPath.
//
// Every write goes through an *os.Root opened on that directory, which resolves
// each path component beneath it and refuses one that leaves. That is the
// defense, and a lexical check cannot be: an archive is a file that arrived
// from somewhere else, and the entry that escapes is not the one with ".." in
// its name. It is a symlink this restore wrote a moment ago, from an earlier
// entry in the same archive -- `data/x -> /etc`, then `data/x/passwd` -- which
// is a name entirely inside the tree naming a file entirely outside it. The
// pool agent is root on the pool host, so following one writes anywhere.
//
// The lexical check in sandboxtree.EntryName stays, for the different job it does:
// keeping an archive from carrying a `config/` or a `.discobox-archived` over
// what the create is about to write.
//
// The archive's SHA256SUMS is verified at its end, after the files are written,
// because holding a workspace back until it had been checked would mean holding
// it. A tree that fails the check is an error like any other, and ImportTree
// removes it.
func readTree(ctx context.Context, r io.Reader, rootPath string) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()

	reader := tarsums.NewReader(r)
	// Directory metadata is applied last: writing a file into a directory
	// updates that directory's mtime, and restoring a read-only directory
	// before its contents makes the contents unwritable.
	var dirs []*tar.Header
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, err := sandboxtree.EntryName(header.Name)
		if err != nil {
			return err
		}
		if err := readTreeEntry(reader, header, root, name); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			dirs = append(dirs, header)
		}
	}
	// Deepest first, so a parent's mtime is not moved by restoring a child's.
	for i := len(dirs) - 1; i >= 0; i-- {
		name, err := sandboxtree.EntryName(dirs[i].Name)
		if err != nil {
			return err
		}
		if err := restoreMetadata(root, name, dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func readTreeEntry(reader io.Reader, header *tar.Header, root *os.Root, name string) error {
	switch header.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o700); err != nil {
			return err
		}
		// Metadata is applied in the second pass; ownership is not, because a
		// directory written into by a later entry keeps the ownership set here.
		return root.Lchown(name, header.Uid, header.Gid)
	case tar.TypeReg:
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		handle, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		// Bounded by the length the header promised. A tar reader stops there
		// anyway; saying so makes the bound this restore's own rather than
		// something inherited from whoever produced the archive.
		if _, err := io.Copy(handle, io.LimitReader(reader, header.Size)); err != nil {
			handle.Close()
			return err
		}
		// Through the open handle rather than by name: nothing can be swapped
		// for a symlink between the write and the chown if the file is never
		// looked up again.
		if err := restoreFileMetadata(handle, header); err != nil {
			handle.Close()
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
		if header.ModTime.IsZero() {
			return nil
		}
		return root.Chtimes(name, header.ModTime, header.ModTime)
	case tar.TypeSymlink:
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		// The link's target is written verbatim, including one that points
		// outside the tree: it is resolved inside the sandbox, where the tree is
		// mounted somewhere else entirely, so rewriting or refusing it here
		// would break links that work. Writing it is safe because nothing in
		// this restore follows it -- os.Root resolves every later entry's path
		// itself and refuses one that leaves the tree.
		if err := root.Symlink(header.Linkname, name); err != nil {
			return err
		}
		return root.Lchown(name, header.Uid, header.Gid)
	case tar.TypeLink:
		source, err := sandboxtree.EntryName(header.Linkname)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		// Root.Link takes both names relative to the root and links a symlink
		// source as the link itself rather than its target, so a hard link
		// cannot reach a host file either.
		return root.Link(source, name)
	default:
		// Whatever a future exporter adds, and whatever a hand-made archive
		// holds. Skipping keeps a restore from failing on an entry it does not
		// need; the reader has already advanced past the body.
		return nil
	}
}

// restoreFileMetadata applies ownership and mode to a file through its open
// handle.
func restoreFileMetadata(handle *os.File, header *tar.Header) error {
	if err := handle.Chown(header.Uid, header.Gid); err != nil {
		return err
	}
	// After the chown: on Linux, chown clears setuid and setgid.
	return handle.Chmod(header.FileInfo().Mode().Perm())
}

func restoreMetadata(root *os.Root, name string, header *tar.Header) error {
	if err := root.Lchown(name, header.Uid, header.Gid); err != nil {
		return err
	}
	// After the chown: on Linux, chown clears setuid and setgid.
	if err := root.Chmod(name, header.FileInfo().Mode().Perm()); err != nil {
		return err
	}
	if header.ModTime.IsZero() {
		return nil
	}
	return root.Chtimes(name, header.ModTime, header.ModTime)
}
