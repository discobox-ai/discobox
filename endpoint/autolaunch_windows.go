//go:build windows

package endpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// acquireLaunchLock takes an exclusive lock on path, waiting for whoever holds
// it. LockFileEx without LOCKFILE_FAIL_IMMEDIATELY blocks, which is what the
// flock on Unix does and what this needs: the caller is about to decide whether
// to start a server, and two callers deciding at once start two.
//
// Opening the file used to be the whole of it, which locked nothing.
func acquireLaunchLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		var unlockOverlapped windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 1, 0, &unlockOverlapped)
		_ = f.Close()
	}, nil
}

// setDetachedProcess cuts the launched server loose from the console that
// started it.
//
// This was empty, which was invisible for as long as the autolaunch was
// launching a command that did not exist. A child sharing its parent's console
// is in that console's process group: Ctrl+C in the terminal reaches it, and
// closing the window sends it CTRL_CLOSE_EVENT. A server started in the
// background so it can outlive the command that wanted it must be in neither.
//
// DETACHED_PROCESS rather than a new console, so no window appears. Its stdio
// is already redirected to the launch log, so it has nothing a console is for.
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func startUserService(context.Context, LaunchOptions) (bool, error) {
	return false, nil
}
