// Package wsl is how this CLI asks the questions only Windows can answer when
// it runs inside a WSL distribution: whether it does, how a path is spelled on
// the other side, where a Windows program is, and what a Windows environment
// variable holds. Every package that crosses into Windows from WSL goes
// through here, so detection and the interop fallbacks are the same for all of
// them.
package wsl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ProbeTimeout bounds the calls that cross into Windows. They are quick when
// interop works at all, and when it does not they are the difference between
// an error and a command that never returns.
const ProbeTimeout = 15 * time.Second

// Detected reports whether this process runs inside a WSL distribution with
// Windows to reach: WSL sets WSL_DISTRO_NAME and WSL_INTEROP in every process
// it starts there.
//
// The kernel's release string is not asked. It says "microsoft" in every
// container on a WSL2 kernel too — Docker Desktop's, a discobox on a wslc pool
// — where there is no cmd.exe to run and no Windows profile to write; and
// clearing the two variables is how a test keeps this from crossing into the
// developer's real Windows side.
func Detected() bool {
	return strings.TrimSpace(os.Getenv("WSL_DISTRO_NAME")) != "" || strings.TrimSpace(os.Getenv("WSL_INTEROP")) != ""
}

// PowerShell is powershell.exe, to run from this side: wherever PATH has it,
// or at the path Windows always installs it.
func PowerShell(ctx context.Context) (string, error) {
	return Command(ctx, "powershell.exe", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`)
}

// WindowsPath is how Windows spells a path in this distribution.
func WindowsPath(ctx context.Context, p string) (string, error) {
	return wslpath(ctx, "-w", p)
}

// LinuxPath is how this distribution spells a Windows path.
func LinuxPath(ctx context.Context, p string) (string, error) {
	return wslpath(ctx, "-u", p)
}

func wslpath(ctx context.Context, mode, p string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wslpath", mode, p).Output()
	if err != nil {
		return "", fmt.Errorf("wslpath %s %s: %w", mode, p, err)
	}
	translated := strings.TrimSpace(string(out))
	if translated == "" {
		return "", fmt.Errorf("wslpath %s %s: no path returned", mode, p)
	}
	return translated, nil
}

// Command finds a Windows program to run from this side. It is normally on
// PATH, since WSL appends the Windows one to it — but a distribution
// configured with appendWindowsPath=false has none of that PATH and the
// program is still there. Its Windows path is known; where the drive holding
// it is mounted is not, and wslpath knows.
func Command(ctx context.Context, name, windowsPath string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return LinuxPath(ctx, windowsPath)
}

// Env asks Windows for one of its own environment variables.
//
// It is asked through cmd.exe rather than read from this process's
// environment, because the Windows environment is not inherited into a WSL
// shell. The stderr cmd.exe writes when it dislikes the working directory —
// this process's, which is not a drive path — is captured and dropped: it
// warns, then answers anyway.
func Env(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	interpreter, err := Command(ctx, "cmd.exe", `C:\Windows\System32\cmd.exe`)
	if err != nil {
		return "", err
	}
	//nolint:gosec // G204: name is a caller's constant, not input.
	out, err := exec.CommandContext(ctx, interpreter, "/c", "echo %"+name+"%").Output()
	if err != nil {
		return "", fmt.Errorf("ask Windows for %%%s%%: %w", name, err)
	}
	value := strings.TrimSpace(string(out))
	// cmd.exe echoes an unset variable back as its own name in percent signs.
	if value == "" || strings.Contains(value, "%") {
		return "", fmt.Errorf("no %%%s%% in the Windows environment", name)
	}
	return value, nil
}
