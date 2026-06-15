//go:build !linux

package systemd

import (
	"context"
	"log/slog"
	"os/exec"
)

func StartNamespace(context.Context, *slog.Logger) (*exec.Cmd, error) {
	return nil, nil
}

func StartChildReaper(context.Context, *slog.Logger, map[int]string) func() {
	return func() {}
}

func ManagedChildProcesses(*exec.Cmd) map[int]string {
	return nil
}

func ExecSystemdChildIfRequested() error {
	return nil
}

func Stop(*exec.Cmd) {}
