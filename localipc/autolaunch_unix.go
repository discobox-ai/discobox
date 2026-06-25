//go:build !windows

package localipc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func acquireLaunchLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func startUserService(ctx context.Context, opts LaunchOptions) (bool, error) {
	if !systemdUserManagerAvailable(ctx) {
		return false, nil
	}
	args := systemdRunArgs(opts)
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	_, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	return true, nil
}

func systemdUserManagerAvailable(ctx context.Context) bool {
	if _, lookupErr := exec.LookPath("systemd-run"); lookupErr != nil {
		return false
	}
	if _, lookupErr := exec.LookPath("systemctl"); lookupErr != nil {
		return false
	}
	return exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Run() == nil
}

func systemdRunArgs(opts LaunchOptions) []string {
	args := []string{
		"--user",
		"--collect",
		"--unit=" + userServiceUnitName(opts),
		"--property=Description=Discobox local API server",
	}
	for _, entry := range opts.Env {
		args = append(args, "--setenv="+entry)
	}
	args = append(args, "--", opts.Command)
	args = append(args, opts.Args...)
	return args
}
