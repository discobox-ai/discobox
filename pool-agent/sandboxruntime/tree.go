package sandboxruntime

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A sandbox's durable tree is the half of it that outlives its container: the
// subtrees a create reuses rather than rebuilds (ADR 0022 §6). Exporting it and
// restoring it elsewhere is what makes a discobox portable (ADR 0123).
//
// The tree travels as a plain tar with relative names, because that is the
// format two pool agents of different versions can agree on without a contract
// between them.

// treeSubtrees is exactly what travels, and the list is the decision rather
// than a convenience (ADR 0123 §1).
//
//   - data is the sandbox user's home: everything they did that was not a
//     commit.
//   - sources is the workspace, git objects and all.
//   - origins is the bare repository of each push-delivered source, without
//     which a restored push-delivered sandbox has no `origin` to push back to.
//
// config and secrets are deliberately absent. Both are written in full by the
// create that follows a restore -- writeSandboxHarnessConfig rewrites the
// sandbox document and refreshSourcesReady its sibling, writeSandboxSecrets the
// secrets one -- so carrying them would move only material the destination
// regenerates, and that material is this pool's: sentinels minted here, and a
// harness document naming this pool's proxy.
var treeSubtrees = []string{"data", "sources", "origins"}

// ErrTreeExists refuses a restore onto a sandbox this pool already holds.
// Overwriting would merge two sandboxes' data into one tree, and there is no
// reading of "import" that means that.
var ErrTreeExists = errors.New("sandbox data already exists on this pool")

// ErrSandboxRunning refuses to read a tree out from under a running container.
// A tar of a live tree can catch a git index mid-write or a sqlite file between
// its journal and its pages, and the reader finds out only on the far side of a
// transfer (ADR 0123 §2).
var ErrSandboxRunning = errors.New("sandbox is running; stop it before exporting it")

// ExportTree streams the sandbox's durable tree as a tar archive.
//
// The walk happens while the caller reads, through a pipe, because the tree is
// gigabytes of workspace and nothing here should hold it. A failure part way
// through therefore cannot be a status: it reaches the caller as a read error
// on a body that has already begun, which is the same bargain every streaming
// route in this repository makes.
func (r *DockerSandboxRuntime) ExportTree(ctx context.Context, sandboxID string) (io.ReadCloser, error) {
	root := r.sandboxRoot(sandboxID)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, ErrNotFound)
		}
		return nil, fmt.Errorf("export sandbox %s: %w", sandboxID, err)
	}
	// Asked before a byte is written, so "it is running" is a status and not a
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

	reader, writer := io.Pipe()
	go func() {
		err := writeTree(ctx, writer, root)
		// CloseWithError(nil) is Close, so one call covers both outcomes and the
		// reader sees the walk's failure rather than a clean end of archive.
		_ = writer.CloseWithError(err)
	}()
	return reader, nil
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

// writeTree tars the subtrees under root that travel.
func writeTree(ctx context.Context, w io.Writer, root string) error {
	writer := tar.NewWriter(w)
	links := newLinkIndex()
	for _, subtree := range treeSubtrees {
		source := filepath.Join(root, subtree)
		if _, err := os.Lstat(source); err != nil {
			if os.IsNotExist(err) {
				// A sandbox with no push-delivered source has no origins
				// directory, and one whose create never got as far as its
				// volumes may have none of them. Absent is not empty and not an
				// error: what is there is what travels.
				continue
			}
			return err
		}
		if err := writeSubtree(ctx, writer, root, subtree, links); err != nil {
			return err
		}
	}
	return writer.Close()
}

func writeSubtree(ctx context.Context, writer *tar.Writer, root, subtree string, links *linkIndex) error {
	source := filepath.Join(root, subtree)
	return filepath.Walk(source, func(file string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		name, err := exportEntryName(root, file)
		if err != nil {
			return err
		}
		return writeTreeEntry(ctx, writer, file, name, info, links)
	})
}

// writeTreeEntry emits one file, and is where everything a sandbox's home can
// hold that a tar cannot is dealt with.
func writeTreeEntry(ctx context.Context, writer *tar.Writer, file, name string, info os.FileInfo, links *linkIndex) error {
	mode := info.Mode()
	switch {
	case mode.IsDir(), mode.IsRegular(), mode&os.ModeSymlink != 0:
	default:
		// Sockets, fifos and device nodes. A sandbox's home routinely holds the
		// first two -- an ssh-agent, a language server, a dev server's control
		// socket -- and none of them mean anything once the process that made
		// them is gone. Skipping is silent because it is expected; naming each
		// one would bury a real problem under a page of them.
		return nil
	}

	var link string
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(file)
		if err != nil {
			return err
		}
		link = target
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = name
	if mode.IsDir() {
		header.Name += "/"
	}
	// Uname/Gname resolve against this host's passwd file, which is the pool
	// container's and not the sandbox's. The numeric ids are what the
	// destination restores and what the sandbox user is addressed by
	// (sandboxuser), so the names are dropped rather than carried to be ignored.
	header.Uname, header.Gname = "", ""

	if mode.IsRegular() {
		if target, ok := links.seen(info, name); ok {
			// A second name for one inode. pnpm's store and git's alternates
			// both do this at scale, and writing the bytes again would inflate
			// an export by however many times the file is linked.
			header.Typeflag = tar.TypeLink
			header.Linkname = target
			header.Size = 0
			return writer.WriteHeader(header)
		}
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if !mode.IsRegular() {
		return nil
	}
	return copyTreeFile(ctx, writer, file, header.Size)
}

// copyTreeFile writes one regular file's contents at exactly the length its
// header promised, and fails the export if the file is no longer that length.
//
// A sandbox is stopped but its tree is not frozen: a pool-side reaper, or a
// container started out of band, can change a file between the walk that sized
// it and the read that sends it. The header is written by then, so the entry
// cannot change length -- an archive whose bodies do not match their headers is
// unreadable, and one changed file would cost the whole export.
//
// Ending the archive is not the same as desynchronizing it, though. So a file
// that shrank or grew ends it: the walk returns, the tar writer is never
// closed, and the reader gets the unexpected EOF that any failure part way
// through a walk produces. The user retries. The alternative is a git pack or a
// sqlite file restored zero-padded onto the destination, discovered from inside
// the sandbox, after a transfer has already archived the source -- which is
// exactly the damage ADR 0123 §2 refuses a running sandbox to avoid.
//
// A file that *vanished* is the exception and is padded rather than fatal.
// Cache files under a home directory come and go, and failing a whole export
// because one was collected would be absurd; a file that is gone also takes
// nothing corrupt with it.
func copyTreeFile(ctx context.Context, writer io.Writer, file string, size int64) error {
	handle, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			// Deleted between the walk and here. The header is already written,
			// so the entry has to be filled; zeros are the only thing left to
			// fill it with.
			_, writeErr := io.Copy(writer, zeroReader(size))
			return writeErr
		}
		return err
	}
	defer handle.Close()
	written, err := io.Copy(writer, io.LimitReader(withContext(ctx, handle), size))
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("%s shrank from %d to %d bytes while it was being exported", file, size, written)
	}
	// The LimitReader above stops at the promised length, so a file that grew
	// reads as a clean copy of a prefix. Ask the handle rather than trust that.
	if current, err := handle.Stat(); err == nil && current.Size() > size {
		return fmt.Errorf("%s grew from %d to %d bytes while it was being exported", file, size, current.Size())
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
// The lexical check in treeEntryName stays, for the different job it does:
// keeping an archive from carrying a `config/` or a `.discobox-archived` over
// what the create is about to write.
func readTree(ctx context.Context, r io.Reader, rootPath string) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()

	reader := tar.NewReader(r)
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
		name, err := treeEntryName(header.Name)
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
		name, err := treeEntryName(dirs[i].Name)
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
		source, err := treeEntryName(header.Linkname)
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

// exportEntryName is the archive name of a file under the sandbox root: a
// relative, slash-separated path, which is what makes the archive portable
// between two pools that hold the same sandbox at different absolute paths.
func exportEntryName(root, file string) (string, error) {
	relative, err := filepath.Rel(root, file)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

// treeEntryName is an archive entry's name as a path relative to the sandbox
// tree, and refuses one that is not part of a sandbox tree at all.
//
// This is not the containment check -- os.Root is (see readTree). What it is
// for is the three subtrees: everything else under the sandbox root is written
// by this pool for itself, and an archive carrying a `config/sandbox.json` or a
// `.discobox-archived` would overwrite what the create is about to produce.
func treeEntryName(name string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("archive entry %q names no file", name)
	}
	top, _, _ := strings.Cut(clean, "/")
	if !travelingSubtree(top) {
		return "", fmt.Errorf("archive entry %q is not part of a sandbox tree", name)
	}
	return clean, nil
}

func travelingSubtree(name string) bool {
	for _, subtree := range treeSubtrees {
		if name == subtree {
			return true
		}
	}
	return false
}

// zeroReader fills n bytes with zeros, for an entry whose file went away after
// its header was written.
func zeroReader(n int64) io.Reader {
	return io.LimitReader(zeroes{}, n)
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// withContext ends a long copy when the caller goes away. A single file can be
// gigabytes, and io.Copy on its own has no reason to stop.
func withContext(ctx context.Context, r io.Reader) io.Reader {
	return &contextReader{ctx: ctx, reader: r}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.reader.Read(p)
}
