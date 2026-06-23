//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func processExists(pid int) bool {
	if processIsZombie(pid) {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminateDaemonProcessGroup(pid int) error {
	return signalDaemonProcessGroup(pid, syscall.SIGTERM)
}

func killDaemonProcessGroup(pid int) error {
	return signalDaemonProcessGroup(pid, syscall.SIGKILL)
}

func isProcessNotFound(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func signalDaemonProcessGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}

func acquireStartupLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func startDetachedDaemon(ctx context.Context, paths sessionPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "--session-id", paths.SessionID, "--repo-root", paths.RepoRoot, "daemon", "--foreground")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "DISCOBOX_SESSION_ID="+paths.SessionID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
