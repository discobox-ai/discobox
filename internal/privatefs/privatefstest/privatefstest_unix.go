//go:build !windows

package privatefstest

import (
	"os"
	"testing"
)

// Widen makes path readable by everyone: 0755 for a directory, 0644 for a
// file.
func Widen(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	mode := os.FileMode(0o644)
	if info.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("widen %s: %v", path, err)
	}
}
