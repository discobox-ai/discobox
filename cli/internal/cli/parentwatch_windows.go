//go:build windows

package cli

import "golang.org/x/sys/windows"

// parentProcessAlive reports whether the process that spawned this one is still
// running.
//
// Windows does not reparent an orphan, so the parent pid recorded for this
// process keeps naming a process that has exited and the Unix check would never
// fire. The process is probed directly instead: a handle that cannot be opened
// is one that is gone, and one that is already signaled has exited.
//
// A pid Windows has since handed to something else reads as alive, which keeps
// this invocation running rather than ending it early. That is the right way
// for the mistake to go, and the window for it is the launcher's own lifetime.
func parentProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true
	}
	return state != uint32(windows.WAIT_OBJECT_0)
}
