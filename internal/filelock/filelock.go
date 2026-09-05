// Package filelock takes advisory exclusive locks on files, for resources that
// tolerate exactly one process at a time: a server's data directory, a
// development loop's checkout.
//
// The lock is a file lock rather than a PID file because the kernel releases it
// when the holder dies, however it dies. A crashed holder cannot leave a lock
// that blocks every future start.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrBusy reports that another process holds the lock. Platform files translate
// their native "would block" error to this.
var ErrBusy = errors.New("lock held by another process")

// Lock is a held file lock. Release drops it; so does the process exiting.
type Lock struct {
	file *os.File
}

// TryAcquire takes the lock at path without blocking, creating the file if it
// does not exist, and records this process's PID in it so a caller that loses
// the race can name the holder. It returns ErrBusy when another process holds
// the lock. The directory must already exist.
func TryAcquire(path string) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFileNB(file); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrBusy) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Best-effort: the PID only feeds a waiter's log message, so failing to
	// record it must not fail an otherwise good lock.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
		_ = file.Sync()
	}
	return &Lock{file: file}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	err := unlockFile(l.file)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// HolderPID reads the PID recorded by the process holding the lock at path. It
// reads without the lock, so the answer is advisory: it may be stale or absent,
// and a caller must not depend on it for anything but a message. Windows denies
// readers a byte-range lock's file, so there it reports false while the lock is
// held.
func HolderPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
