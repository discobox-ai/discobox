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
// started it, and gives it one of its own with no window.
//
// This was empty, which was invisible for as long as the autolaunch was
// launching a command that did not exist. A child sharing its parent's console
// is in that console's process group: Ctrl+C in the terminal reaches it, and
// closing the window sends it CTRL_CLOSE_EVENT. A server started in the
// background so it can outlive the command that wanted it must be in neither.
//
// CREATE_NO_WINDOW rather than DETACHED_PROCESS. Both cut the server off from
// the caller's console and differ only in what it has instead: DETACHED_PROCESS
// leaves it with no console at all, CREATE_NO_WINDOW gives it one of its own
// whose window does not exist. That difference is invisible in the server and
// visible in everything it starts, because Windows gives a process that has no
// console a fresh one, window and all, for every console program it runs — and
// the server runs them without meaning to. Resolving a registry credential
// runs whichever docker-credential-*.exe the user's Docker config names, so a
// first start, which inspects one image per built-in harness, put a console
// window on screen for each of them. A child inherits the console it was
// started from, so a windowless one makes all of those windowless too. The
// server's own stdio goes to the launch log either way: the console is for its
// children, not for it.
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

func startUserService(context.Context, LaunchOptions) (bool, error) {
	return false, nil
}
