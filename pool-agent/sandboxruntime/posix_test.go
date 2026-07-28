package sandboxruntime

import (
	"runtime"
	"testing"
)

// requirePOSIXHost skips a test that can only be meaningful on a POSIX host.
//
// Three distinct things break on Windows, none of which say anything about the
// code under test:
//
//   - file ownership: os.Chown is unsupported, and sandbox material is staged
//     with an explicit uid/gid;
//   - absolute paths: the guest paths these exercise ("/host", "/home/...")
//     are not absolute to filepath, which resolves against the running OS;
//   - git: a Windows checkout applies its own line-ending translation, so
//     content pushed as "x\n" is read back as "x\r\n".
//
// The pool agent itself only ever runs inside a Linux container, so none of
// this is a gap in what ships -- it is a gap in where the tests can run.
func requirePOSIXHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX host: file ownership, absolute guest paths, and git line endings")
	}
}
