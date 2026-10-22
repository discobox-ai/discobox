package files

import (
	"errors"
	"syscall"
)

// errorDirNotEmpty is ERROR_DIR_NOT_EMPTY, what RemoveDirectory answers for
// a directory that holds something. Go maps no Windows error to ENOTEMPTY.
const errorDirNotEmpty = syscall.Errno(145)

// notEmpty reports whether removing a directory failed because it holds
// something.
func notEmpty(err error) bool {
	return errors.Is(err, errorDirNotEmpty)
}

// inTheWay reports whether a rename failed because of what is at its
// destination. On Windows that is never told: a directory in the way and a
// refused permission are both ERROR_ACCESS_DENIED, and clearing the
// destination on a refusal would lose it. A replacing move there replaces a
// file, which MoveFileEx does itself, and nothing else.
func inTheWay(error) bool { return false }
