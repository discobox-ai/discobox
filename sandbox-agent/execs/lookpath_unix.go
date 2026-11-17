//go:build !windows

package execs

import (
	"os"
	"path/filepath"
)

// executableIn is dir's program, when it is a file with an executable bit.
// PATHEXT means nothing here.
func executableIn(dir, program, _ string) (string, bool) {
	candidate := filepath.Join(dir, program)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", false
	}
	return candidate, true
}

// envNameEqual compares environment names as this platform does: exactly.
func envNameEqual(a, b string) bool {
	return a == b
}
