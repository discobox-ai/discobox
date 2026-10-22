package ahphost

import (
	"context"
	"errors"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// Workspace is a discobox's own filesystem and git, which only a host running
// in that discobox has (ADR 26-10-22-853 §1): the sandbox agent gives it one,
// and the CLI's host, across discoboxes, has none (Config.Workspace is nil).
// A host with a Workspace serves the one discobox it runs in, with its
// working tree's files and changesets. Paths are absolute paths on this
// machine; errors are the fs package's (fs.ErrNotExist, fs.ErrExist,
// fs.ErrPermission, fs.ErrInvalid) or ErrChanged.
type Workspace interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, write FileWrite) error
	// ReadDir lists a directory, a symlink by what it points at.
	ReadDir(path string) ([]DirEntry, error)
	// Stat describes a file, following symlinks when follow is set; Path is
	// then the resolved path.
	Stat(path string, follow bool) (FileInfo, error)
	// Mkdir makes a directory and its parents.
	Mkdir(path string) error
	Remove(path string, recursive bool) error
	// Copy and Move put from at to, replacing what is there only when
	// replace is set. Neither loses to when it fails.
	Copy(from, to string, replace bool) error
	Move(from, to string, replace bool) error
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

// ErrChanged is a write whose IfMatch no longer names the file's version.
var ErrChanged = errors.New("the file has changed since it was read")

// FileWrite is what resourceWrite asks to write: Data placed at Position
// as Mode says (truncate from there, insert there, or insert that far back
// from the end).
type FileWrite struct {
	Data       []byte
	Mode       ahptypes.ResourceWriteMode
	Position   int64
	CreateOnly bool
	// IfMatch, when set, is the version (FileInfo.Etag) the file must still
	// be at.
	IfMatch string
}

type DirEntry struct {
	Name string
	Dir  bool
}

type FileInfo struct {
	Path    string
	Type    ahptypes.ResourceType
	Size    int64
	ModTime time.Time
	Etag    string
}

// Changed is one file a changeset holds: on the before side when the base
// had it, the after side when the working tree does, with its line counts
// when it has lines to count.
type Changed struct {
	Path           string
	Before, After  bool
	Added, Removed *int64
}
