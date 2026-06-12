package workeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
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
	return syscall.Exec(defaultSystemdPath, []string{defaultSystemdPath}, childEnv())
}

func startSystemdNamespace(ctx context.Context, logger *slog.Logger) (*exec.Cmd, error) {
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

func startChildReaper(ctx context.Context, logger *slog.Logger, managed map[int]string) func() {
	if os.Getpid() != 1 {
		return func() {}
	}
	reaperCtx, cancel := context.WithCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, syscall.SIGCHLD)
	go func() {
		defer close(done)
		reapChildren(logger, managed)
		for {
			select {
			case <-reaperCtx.Done():
				reapChildren(logger, managed)
				return
			case <-sigCh:
				reapChildren(logger, managed)
			}
		}
	}()
	return func() {
		cancel()
		signal.Stop(sigCh)
		<-done
	}
}

func reapChildren(logger *slog.Logger, managed map[int]string) {
	for {
		var status syscall.WaitStatus
		var usage syscall.Rusage
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, &usage)
		switch {
		case pid > 0:
			if name := managed[pid]; name != "" {
				logger.Info("managed child exited", "name", name, "pid", pid, "status", waitStatus(status))
				continue
			}
			logger.Info("reaped child process", "pid", pid, "status", waitStatus(status))
		case pid == 0:
			return
		case errors.Is(err, syscall.ECHILD):
			return
		case err != nil:
			logger.Warn("failed to reap child process", "error", err)
			return
		default:
			return
		}
	}
}

func managedChildProcesses(systemd *exec.Cmd) map[int]string {
	if systemd == nil || systemd.Process == nil {
		return nil
	}
	return map[int]string{systemd.Process.Pid: "systemd"}
}

func waitStatus(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return "exited:" + strconv.Itoa(status.ExitStatus())
	case status.Signaled():
		return "signaled:" + status.Signal().String()
	case status.Stopped():
		return "stopped:" + status.StopSignal().String()
	default:
		return "unknown"
	}
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
