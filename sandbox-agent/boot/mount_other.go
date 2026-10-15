//go:build !linux

package boot

import (
	"fmt"
	"os"
)

// Wiring only runs inside a Linux sandbox container.
// These stubs let the package compile on other platforms (for tooling and the
// pure planning tests) without pulling in Linux mount syscalls.

var errUnsupported = fmt.Errorf("wiring a sandbox is only supported on linux")

func bindMount(string, string, bool) error    { return errUnsupported }
func recursiveBindMount(string, string) error { return errUnsupported }
func overlayMount(string, string, string, string) error {
	return errUnsupported
}

// fileOwner cannot report ownership here, so callers copy the mode alone.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
