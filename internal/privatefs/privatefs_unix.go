//go:build !windows

package privatefs

import (
	"fmt"
	"os"
)

// Restrict is what the mode bits already did. A file created 0600 and a
// directory created 0700 are this user's alone, so there is nothing to repair.
func Restrict(string) error { return nil }

// Check reports an error naming the mode when anyone but the owner can reach
// path.
func Check(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s permissions = %o, reachable by others than its owner", path, perm)
	}
	return nil
}
