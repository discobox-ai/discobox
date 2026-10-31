//go:build windows

package sharedfile

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// Retry runs op until it succeeds or fails for another reason than the file
// being held: ERROR_SHARING_VIOLATION and ERROR_ACCESS_DENIED are retried for
// up to SharingTimeout, and ERROR_FILE_NOT_FOUND — a replace can hide the name
// from a reader for an instant — for up to missing, which is zero for a caller
// to whom a missing file is an answer. Any other error is returned at once.
func Retry(op func() error, missing time.Duration) error {
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
			limit = SharingTimeout
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			limit = missing
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
