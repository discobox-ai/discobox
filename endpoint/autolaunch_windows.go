//go:build windows

package endpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

func acquireLaunchLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func setDetachedProcess(*exec.Cmd) {}

func startUserService(context.Context, LaunchOptions) (bool, error) {
	return false, nil
}
