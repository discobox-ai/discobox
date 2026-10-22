package ahphost

import (
	"context"
	"errors"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A discobox's files are reached through Boxes, as the discobox's default
// user (ADR 26-10-22-499): the CLI over the control plane's files API, a host
// in the discobox in its own process, which already is that user. Paths are
// absolute and slash-separated. A file operation's errors are the fs
// package's (fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, fs.ErrInvalid)
// or ErrChanged, wrapped.

// ErrChanged is a write that named a version the file is no longer at.
var ErrChanged = errors.New("the file has changed since it was read")

// FileType is what a path is. A symbolic link is reported as itself.
type FileType string

const (
	FileTypeFile      FileType = "file"
	FileTypeDirectory FileType = "directory"
	FileTypeSymlink   FileType = "symlink"
	FileTypeOther     FileType = "other"
)

// FileInfo is one path in a discobox.
type FileInfo struct {
	// Path is the absolute path, cleaned.
	Path    string
	Name    string
	Type    FileType
	Size    int64
	ModTime time.Time
	// Version changes whenever the content does; a write may name it to
	// replace only what was read.
	Version string
	// LinkTarget is what a symlink points to, as written in it.
	LinkTarget string
}

// FileWrite is a whole file's new content, and what must hold for it to be
// written: that no file is there, or that the file is still at a version.
type FileWrite struct {
	Data       []byte
	CreateOnly bool
	IfVersion  string
}

// Workspace is what only a host running in the discobox it serves has (ADR
// 26-10-22-853 §1): its working tree's resource watches and its git. The
// sandbox agent gives one; the CLI's host, across discoboxes, has none
// (Config.Workspace is nil), and a host with one serves only the discobox it
// runs in.
type Workspace interface {
	// Watch reports changes under root — below it too when recursive — in
	// batches, except those excludes match and, when includes are given,
	// those they do not. Relative globs, `**/` matching at any depth.
	Watch(root string, recursive bool, excludes, includes []string, report func([]ahptypes.ResourceChange)) (stop func(), err error)
	// Changes is what the working tree dir is in holds that base does not:
	// its tracked changes and its untracked files.
	Changes(ctx context.Context, dir, base string) ([]Changed, error)
	// ReadRevision reads a file as revision rev of its repository has it.
	ReadRevision(ctx context.Context, path, rev string) ([]byte, error)
}

// Changed is one file a changeset holds: on the before side when the base
// had it, the after side when the working tree does, with its line counts
// when it has lines to count.
type Changed struct {
	Path           string
	Before, After  bool
	Added, Removed *int64
}
