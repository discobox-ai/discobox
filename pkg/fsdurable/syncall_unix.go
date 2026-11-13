//go:build unix

package fsdurable

import "syscall"

// SyncAll flushes every filesystem this kernel holds, for a write made
// through something else than this process (a Docker daemon copying into a
// container's layer), whose files and directories it cannot name.
func SyncAll() {
	syscall.Sync()
}
