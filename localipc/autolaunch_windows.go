//go:build windows

package localipc

import (
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
