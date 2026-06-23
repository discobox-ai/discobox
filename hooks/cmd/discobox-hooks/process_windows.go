//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

func processExists(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}

func terminateDaemonProcessGroup(pid int) error {
	return killDaemonProcessGroup(pid)
}

func killDaemonProcessGroup(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

func isProcessNotFound(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}

func acquireStartupLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
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
	return cmd.Start()
}
