//go:build !linux

package boot

import (
	"fmt"
	"os"
)

// The sandbox-agent init flow only runs inside the Linux sandbox container.
// These stubs let the package compile on other platforms (for tooling and the
// pure planning tests) without pulling in Linux mount syscalls.

var errUnsupported = fmt.Errorf("sandbox-agent init is only supported on linux")

func bindMount(string, string, bool) error          { return errUnsupported }
func recursiveBindMount(string, string, bool) error { return errUnsupported }
func overlayMount(string, string, string, string) error {
	return errUnsupported
}
func execInit([]string, []string) error { return errUnsupported }

// fileDevice cannot report a device number here, so callers fall back to
// treating every path as one filesystem.
func fileDevice(os.FileInfo) (uint64, bool) { return 0, false }
