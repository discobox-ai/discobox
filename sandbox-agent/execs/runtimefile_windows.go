//go:build windows

package execs

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	// runtimeSharingTimeout bounds how long a rename or a read waits out the
	// other side holding the file. Either holds it for a moment, so this is
	// only ever reached by something holding it open on purpose.
	runtimeSharingTimeout = 2 * time.Second
	// runtimeMissingTimeout bounds how long a read waits for a file that is
	// not there. A replace can hide the name from a reader for an instant,
	// but most misses are an exec that has no file, and every read of one
	// pays this.
	runtimeMissingTimeout = 20 * time.Millisecond
)

// replaceRuntimeFile renames a staged runtime record over the live one.
// Windows refuses to replace a file another handle has open without
// FILE_SHARE_DELETE, and os.Open never asks for it, so a rename that lands
// while the agent reads the file fails rather than waits. That read is brief:
// retry until it is done, as cmd/go's robustio does. Giving up early would
// drop the write — for the shim, the exit status only it knows.
func replaceRuntimeFile(staged, path string) error {
	return retryRuntimeFile(func() error { return os.Rename(staged, path) })
}

// readRuntimeFile reads a runtime record. A read that races a replace can
// fail the other way round — refused while the rename holds the name, or
// told it is missing — and a failed read is a dropped observation: the
// watcher would miss the shim's exit (#203). Retry it as the rename is.
func readRuntimeFile(path string) ([]byte, error) {
	var data []byte
	err := retryRuntimeFile(func() error {
		var err error
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}

func retryRuntimeFile(op func() error) error {
	start := time.Now()
	delay := time.Millisecond
	for {
		err := op()
		if err == nil {
			return nil
		}
		var limit time.Duration
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_SHARING_VIOLATION):
			limit = runtimeSharingTimeout
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			limit = runtimeMissingTimeout
		default:
			return err
		}
		if time.Since(start) >= limit {
			return err
		}
		time.Sleep(delay)
		delay = min(2*delay, 50*time.Millisecond)
	}
}
