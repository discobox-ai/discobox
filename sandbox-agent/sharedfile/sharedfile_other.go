//go:build !windows

package sharedfile

import "time"

// Retry runs op once: outside Windows another handle never stands in the way
// of a read, a rename or a remove.
func Retry(op func() error, _ time.Duration) error { return op() }
