// Package sandboxtree is a sandbox's durable tree as a tar: which subtrees it
// holds, how a directory is written into one, and how an archive's names are
// judged (ADR 0123, ADR 0129).
//
// The tree travels as a plain tar with relative names, because that is the
// format two agents of different versions can agree on without a contract
// between them. It ends with a SHA256SUMS member (tarsums), which is how a
// reader tells a whole tree from one whose stream was cut short.
//
// It is in the root module because two processes write the same archive: the
// sandbox agent's export mode writes `data` and `sources`, and the pool agent
// verifies that stream, re-emits it, and adds the `origins` it owns.
package sandboxtree

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

	"github.com/discobox-ai/discobox/tarsums"
)

// The subtrees a tree holds, and the list is the decision rather than a
// convenience (ADR 0123 §1).
//
//   - Data is the sandbox's data root: the user's home and every image-declared
//     data path the image lets travel (ADR 0129 §2).
//   - Sources is the workspace, git objects and all.
//   - Origins is the bare repository of each push-delivered source, without
//     which a restored push-delivered sandbox has no `origin` to push back to.
//
// config and secrets are deliberately absent. Both are written in full by the
// create that follows a restore, and both are the pool's own: sentinels minted
// there, and a harness document naming that pool's proxy.
const (
	Data    = "data"
	Sources = "sources"
	Origins = "origins"
)

// Subtrees is every subtree a tree may hold, in the order they are written.
var Subtrees = []string{Data, Sources, Origins}

// EntryName is an archive entry's name as a clean path relative to the sandbox
// tree, and refuses one that is not inside one of the named subtrees — every
// subtree when none are named.
//
// This is not a containment check for a restore; os.Root is. What it is for is
// the subtrees themselves: everything else under a sandbox root is written by
// the pool for itself, and an archive carrying a `config/sandbox.json` or a
// `.discobox-archived` would overwrite what the create is about to produce.
func EntryName(name string, subtrees ...string) (string, error) {
	if len(subtrees) == 0 {
		subtrees = Subtrees
	}
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("archive entry %q names no file", name)
	}
	top, _, _ := strings.Cut(clean, "/")
	for _, subtree := range subtrees {
		if top == subtree {
			return clean, nil
		}
	}
	return "", fmt.Errorf("archive entry %q is not part of a sandbox tree", name)
}

// Writer writes directories into a tree archive.
//
// One Writer serves a whole archive so that a file linked from two subtrees is
// still stored once.
type Writer struct {
	archive *tarsums.Writer
	links   *linkIndex
}

// NewWriter writes into archive. Closing archive, which writes its SHA256SUMS,
// stays the caller's, since the caller may add entries of its own.
func NewWriter(archive *tarsums.Writer) *Writer {
	return &Writer{archive: archive, links: newLinkIndex()}
}

// AddDir writes the directory dir into the archive as name, and everything
// beneath it as name/<relative path>.
//
// skip is asked about each entry's archive name before it is written; a
// directory it names is left out along with everything below it. A nil skip
// writes everything.
func (w *Writer) AddDir(ctx context.Context, dir, name string, skip func(name string) bool) error {
	return filepath.Walk(dir, func(file string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		relative, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		entry := name
		if relative != "." {
			entry = path.Join(name, filepath.ToSlash(relative))
		}
		if skip != nil && skip(entry) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return w.writeEntry(ctx, file, entry, info)
	})
}

// writeEntry emits one file, and is where everything a sandbox's home can hold
// that a tar cannot is dealt with.
func (w *Writer) writeEntry(ctx context.Context, file, name string, info os.FileInfo) error {
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
	// Uname/Gname resolve against the writer's passwd file, which is not the
	// destination's. The numeric ids are what a restore applies and what the
	// sandbox user is addressed by (sandboxuser), so the names are dropped
	// rather than carried to be ignored.
	header.Uname, header.Gname = "", ""

	if mode.IsRegular() {
		if target, ok := w.links.seen(info, name); ok {
			// A second name for one inode. pnpm's store and git's alternates
			// both do this at scale, and writing the bytes again would inflate
			// an export by however many times the file is linked.
			header.Typeflag = tar.TypeLink
			header.Linkname = target
			header.Size = 0
			return w.archive.WriteHeader(header)
		}
	}
	if err := w.archive.WriteHeader(header); err != nil {
		return err
	}
	if !mode.IsRegular() {
		return nil
	}
	return copyFile(ctx, w.archive, file, header.Size)
}

// copyFile writes one regular file's contents at exactly the length its header
// promised, and fails the export if the file is no longer that length.
//
// A tree being exported is not running, but it is not frozen either: something
// started out of band can change a file between the walk that sized it and the
// read that sends it. The header is written by then, so the entry cannot change
// length -- an archive whose bodies do not match their headers is unreadable,
// and one changed file would cost the whole export.
//
// Ending the archive is not the same as desynchronizing it, though. So a file
// that shrank or grew ends it: the walk returns, the archive never gets its
// SHA256SUMS, and the reader refuses it as it refuses any walk that failed part
// way. The alternative is a git pack or a sqlite file restored zero-padded onto
// the destination, discovered from inside the sandbox after a transfer has
// already archived the source (ADR 0123 §2).
//
// A file that *vanished* is the exception and is padded rather than fatal.
// Cache files under a home directory come and go, and failing a whole export
// because one was collected would be absurd; a file that is gone also takes
// nothing corrupt with it.
func copyFile(ctx context.Context, writer io.Writer, file string, size int64) error {
	handle, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			// Deleted between the walk and here. The header is already written,
			// so the entry has to be filled; zeros are the only thing left to
			// fill it with.
			_, writeErr := io.Copy(writer, io.LimitReader(zeroes{}, size))
			return writeErr
		}
		return err
	}
	defer handle.Close()
	written, err := io.Copy(writer, io.LimitReader(&contextReader{ctx: ctx, reader: handle}, size))
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

// Copy re-emits every entry of src into dst, refusing any entry -- or hard link
// target -- outside the named subtrees.
//
// It is how an archive produced by something less trusted than its reader is
// passed on: src verifies its own SHA256SUMS as it is read, so a stream cut
// short or altered is an error here before dst could be closed over it, and the
// names are judged before they are written rather than after.
func Copy(dst *tarsums.Writer, src *tarsums.Reader, subtrees ...string) error {
	for {
		header, err := src.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := EntryName(header.Name, subtrees...)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			name += "/"
		}
		header.Name = name
		if header.Typeflag == tar.TypeLink {
			target, err := EntryName(header.Linkname, subtrees...)
			if err != nil {
				return fmt.Errorf("hard link %q: %w", header.Name, err)
			}
			header.Linkname = target
		}
		if err := dst.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 {
			continue
		}
		written, err := io.Copy(dst, io.LimitReader(src, header.Size))
		if err != nil {
			return err
		}
		if written != header.Size {
			return fmt.Errorf("archive entry %q is %d bytes, header said %d", header.Name, written, header.Size)
		}
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// contextReader ends a long copy when the caller goes away. A single file can
// be gigabytes, and io.Copy on its own has no reason to stop.
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
