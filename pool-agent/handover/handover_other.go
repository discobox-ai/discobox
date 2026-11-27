//go:build !linux

package handover

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"syscall"
)

// Where there are no open file description locks (fcntl(2) F_OFD_SETLK, Linux
// alone), only this process's own locks exclude each other: no other process
// of a pool hands over there.
var (
	localMu    sync.Mutex
	localLocks = map[string]int{} // path: -1 held alone, n > 0 held by n marks
	localFiles = map[*os.File]string{}
)

func tryLock(file *os.File, exclusive bool) (bool, error) {
	localMu.Lock()
	defer localMu.Unlock()
	path := file.Name()
	held := localLocks[path]
	switch {
	case exclusive && held != 0, !exclusive && held < 0:
		return false, nil
	case exclusive:
		localLocks[path] = -1
	default:
		localLocks[path] = held + 1
	}
	localFiles[file] = path
	return true, nil
}

// openPending reports whether a lock file's open failed only for now: on
// Windows a file removed (Remove) while another handle still has it open is
// pending deletion until that handle closes, and opening its path is refused
// meanwhile, as permission denied or as a sharing violation. A waiter's own
// probe in acquire is such a handle, briefly.
func openPending(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	return errors.Is(err, fs.ErrPermission) || errors.As(err, &errno) && errno == windowsSharingViolation
}

// windowsSharingViolation is ERROR_SHARING_VIOLATION.
const windowsSharingViolation = syscall.Errno(32)

// settle closes a held lock's file: the lock is this process's own record,
// which needs no open file, and Windows cannot remove a file held open, which
// a removal under its lock does (Remove).
func settle(file *os.File) { _ = file.Close() }

func unlock(file *os.File) {
	localMu.Lock()
	defer localMu.Unlock()
	path, ok := localFiles[file]
	if !ok {
		return
	}
	delete(localFiles, file)
	if localLocks[path] <= 1 {
		delete(localLocks, path)
		return
	}
	localLocks[path]--
}

// lockHeld reports whether this process holds file's path locked.
func lockHeld(file *os.File) (bool, error) {
	localMu.Lock()
	defer localMu.Unlock()
	return localLocks[file.Name()] != 0, nil
}
