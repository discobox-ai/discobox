//go:build windows

package dockercache

import "os"

// watchResize has no resize signal to subscribe to on Windows, so the child's
// pty keeps the size it was opened with. The shim only ever runs inside a
// Linux sandbox; this exists so the package still builds everywhere.
func watchResize(*os.File) func() { return func() {} }
