// Package shorttmp hands tests a temporary directory short enough to hold a
// Unix socket.
//
// A sockaddr_un path cannot exceed 108 bytes, and t.TempDir() spends that
// budget twice over. It roots at $TMPDIR — which `nix develop` points inside
// the workspace and macOS points at /var/folders/<two levels of base64> — and
// then appends the test's own name. Several packages here bind a socket under
// that directory, and the exec runtime adds an `ex_<id>.sock` of its own
// beneath it, so the tests with the longest names overflow first and the rest
// sit one rename away from doing the same.
//
// This is test-only machinery, but it lives in a normal package because the
// packages that need it cannot share a _test.go file, and in the root module
// because they span two: hooks and sandbox-agent both bind sockets in tests,
// and Go's internal rule is path-prefix based, so both can reach it here.
package shorttmp

import (
	"os"
	"runtime"
	"testing"
)

// Dir returns a temporary directory rooted somewhere short, removed when the
// test ends. Windows named pipes have no path limit, so there it is an ordinary
// temporary directory.
func Dir(t *testing.T) string {
	t.Helper()
	root := ""
	if runtime.GOOS != "windows" {
		root = "/tmp"
	}
	dir, err := os.MkdirTemp(root, "dbx")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
