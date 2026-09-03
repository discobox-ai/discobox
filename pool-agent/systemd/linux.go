//go:build linux

package systemd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const defaultSystemdPath = "/sbin/init"
const systemdChildEnv = "DISCOBOX_SYSTEMD_CHILD"

func ExecSystemdChildIfRequested() error {
	if os.Getenv(systemdChildEnv) != "1" {
		return nil
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return err
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return err
	}
	//nolint:gosec // Path and argv are fixed constants for entering the systemd child process.
	return syscall.Exec(defaultSystemdPath, []string{defaultSystemdPath}, childEnv())
}

func Stop(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

func StartNamespace(ctx context.Context, logger *slog.Logger) (*exec.Cmd, error) {
	if os.Getpid() != 1 {
		return nil, nil
	}
	if _, err := os.Stat(defaultSystemdPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("systemd init not found; continuing without child pid namespace", "path", defaultSystemdPath)
			return nil, nil
		}
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self)
	cmd.Env = append(os.Environ(), systemdChildEnv+"=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	logger.Info("started systemd in child pid namespace", "pid", cmd.Process.Pid)
	return cmd, nil
}

// ManagedChildProcesses names the children this process starts and never waits
// for, for the reaper that collects them. The systemd child is the only one:
// it is stopped with a signal, so it is deliberately not registered with
// childproc and the reaper is what reports its exit.
func ManagedChildProcesses(systemd *exec.Cmd) map[int]string {
	if systemd == nil || systemd.Process == nil {
		return nil
	}
	return map[int]string{systemd.Process.Pid: "systemd"}
}

func childEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, systemdChildEnv+"=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}
