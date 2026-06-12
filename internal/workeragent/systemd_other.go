//go:build !linux

package workeragent

import (
	"context"
	"log/slog"
	"os/exec"
)

func startSystemdNamespace(context.Context, *slog.Logger) (*exec.Cmd, error) {
	return nil, nil
}

func startChildReaper(context.Context, *slog.Logger, map[int]string) func() {
	return func() {}
}

func managedChildProcesses(*exec.Cmd) map[int]string {
	return nil
}

func ExecSystemdChildIfRequested() error {
	return nil
}
