//go:build !windows

package endpoint

import (
	"os"
	"path/filepath"
	"testing"
)

// testSocketPath returns a Unix socket path short enough for the platform's
// sockaddr_un limit — 104 bytes on macOS, 108 on Linux.
//
// t.TempDir() cannot be used for this: it embeds the test's own name in the
// path, and on macOS the private temporary directory is already most of the
// budget before a test name and a file name are appended. The symptom is a
// bind() failing with "invalid argument" in some tests and not others, decided
// by how long the test is called.
func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbx")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}
