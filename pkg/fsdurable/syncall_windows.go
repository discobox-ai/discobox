//go:build windows

package fsdurable

// SyncAll is a no-op on Windows, which has no call that flushes every
// volume; see SyncDir.
func SyncAll() {}
