// Package files is the sandbox agent's files API (ADR 26-10-22-499): stat,
// list, read, write, mkdir, remove, move and copy, each done as the
// discobox's default user and never as the agent's root.
//
// The operations themselves are plain filesystem calls (ops.go). Who makes
// them is the platform's (service_linux.go, service_other.go): on Linux a
// helper process started with the user's credential, which is this binary's
// files-helper mode (helper.go); where a sandbox has one account, the agent,
// which already is that account.
package files

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Op names an operation.
type Op string

const (
	OpStat   Op = "stat"
	OpList   Op = "list"
	OpRead   Op = "read"
	OpWrite  Op = "write"
	OpMkdir  Op = "mkdir"
	OpRemove Op = "remove"
	OpMove   Op = "move"
	OpCopy   Op = "copy"
)

// Request is one operation on resolved, absolute paths (Resolve). It is also
// what the agent hands the helper, as JSON.
type Request struct {
	Op   Op     `json:"op"`
	Path string `json:"path"`
	// To is a move's or a copy's destination.
	To string `json:"to,omitempty"`

	// Offset and Length bound a read; a negative Length reads to the end.
	Offset int64 `json:"offset,omitempty"`
	Length int64 `json:"length,omitempty"`

	// FailIfExists and FailIfMissing refuse a write; IfVersion refuses one
	// unless the file is still at that version.
	FailIfExists  bool   `json:"failIfExists,omitempty"`
	FailIfMissing bool   `json:"failIfMissing,omitempty"`
	IfVersion     string `json:"ifVersion,omitempty"`
	// Mode is the permission bits a write or a mkdir gives what it creates;
	// nil leaves them to the umask.
	Mode *uint32 `json:"mode,omitempty"`
	// Parents creates a write's or a mkdir's missing parents.
	Parents bool `json:"parents,omitempty"`
	// Recursive removes a directory with what it holds.
	Recursive bool `json:"recursive,omitempty"`
	// Replace lets a move or a copy replace its destination.
	Replace bool `json:"replace,omitempty"`
}

// Type is what a path is.
type Type string

const (
	TypeFile      Type = "file"
	TypeDirectory Type = "directory"
	TypeSymlink   Type = "symlink"
	TypeOther     Type = "other"
)

// Stat is one path, as lstat sees it.
type Stat struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       Type      `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Version    string    `json:"version"`
	LinkTarget string    `json:"linkTarget,omitempty"`
}

// Result is what an operation answers, besides a read's content: the path it
// made or was asked about, and a directory's entries.
type Result struct {
	Stat    *Stat  `json:"stat,omitempty"`
	Entries []Stat `json:"entries,omitempty"`
}

// Kind is the class of a failure, which is what the API answers with.
type Kind string

const (
	KindInvalid    Kind = "invalid"
	KindNotFound   Kind = "not-found"
	KindExists     Kind = "exists"
	KindConflict   Kind = "conflict"
	KindPermission Kind = "permission"
	// KindPrecondition is a write its conditions refused: a file where it
	// must create one, or one no longer at the version it names.
	KindPrecondition Kind = "precondition"
	KindInternal     Kind = "internal"
)

// Error is a failed operation. It crosses from the helper to the agent as
// JSON, so it carries its kind rather than the error it came from.
type Error struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func errorf(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// classify turns a filesystem error into an *Error of the kind it is.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	kind := KindInternal
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = KindNotFound
	case errors.Is(err, fs.ErrExist):
		kind = KindExists
	case errors.Is(err, fs.ErrPermission):
		kind = KindPermission
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTEMPTY):
		kind = KindConflict
	}
	return &Error{Kind: kind, Message: err.Error()}
}

// Resolve makes a path the API was given absolute. An absolute path is only
// cleaned. A leading `~` is home, and any other relative path is relative to
// home, as `discobox cp` and the SSH ingress take one.
func Resolve(path, home string) (string, error) {
	if path == "" {
		return "", errorf(KindInvalid, "a path is required")
	}
	if strings.ContainsRune(path, 0) {
		return "", errorf(KindInvalid, "a path may not contain a NUL byte")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	rest := path
	if path == "~" {
		rest = ""
	} else if after, ok := strings.CutPrefix(path, "~/"); ok {
		rest = after
	}
	if strings.TrimSpace(home) == "" {
		return "", errorf(KindInvalid, "%q is relative to the default user's home, and that user has none", path)
	}
	return filepath.Join(home, rest), nil
}
