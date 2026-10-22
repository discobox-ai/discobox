//go:build !windows

package files

import (
	"errors"
	"syscall"
)

// notEmpty reports whether removing a directory failed because it holds
// something: ENOTEMPTY, or EEXIST where POSIX lets rmdir say that instead.
func notEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// inTheWay reports whether a rename failed because of what is at its
// destination: a directory that is not empty, or a file and a directory
// either way round.
func inTheWay(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) ||
		errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR)
}
