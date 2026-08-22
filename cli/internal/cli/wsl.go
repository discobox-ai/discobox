package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WSL is the one place where the program this CLI hands work to does not run on
// the same side of the machine as the CLI. A discobox reached from a Linux
// distribution is opened by a Windows editor, connected to by Windows OpenSSH,
// and configured by files under a Windows profile — none of which can be
// spelled, quoted or executed the way this side spells, quotes and executes
// them. What that costs the emitted ssh_config is in sshTarget; what it takes
// to ask Windows the questions only Windows can answer is here.

// wslDistroEnv is set in every WSL shell, and names the distribution `wsl.exe
// -d` re-enters.
const wslDistroEnv = "WSL_DISTRO_NAME"

// wslProbeTimeout bounds the calls that cross into Windows. They are quick when
// interop works at all, and when it does not they are the difference between an
// error and a command that never returns.
const wslProbeTimeout = 15 * time.Second

// isWSL reports whether this process runs inside a WSL distribution.
func isWSL() bool {
	return strings.TrimSpace(os.Getenv(wslDistroEnv)) != "" || strings.TrimSpace(os.Getenv("WSL_INTEROP")) != ""
}

// isWindowsExecutable reports whether path names a file Windows can run: one on
// a mounted Windows drive rather than in this distribution's own filesystem.
//
// wslpath answers it rather than a /mnt prefix test, because where the drives
// are mounted is configurable (/etc/wsl.conf `root`) — and because the answer
// for a path inside the distribution is unmistakable: it comes back as a
// \\wsl.localhost UNC share, which is exactly the thing Windows can reach but
// should not be asked to.
func isWindowsExecutable(ctx context.Context, path string) bool {
	windows, err := wslWindowsPath(ctx, path)
	return err == nil && !strings.HasPrefix(windows, `\\`)
}

// wslWindowsPath is how Windows spells a path in this distribution.
func wslWindowsPath(ctx context.Context, p string) (string, error) {
	return wslpath(ctx, "-w", p)
}

// wslLinuxPath is how this distribution spells a Windows path.
func wslLinuxPath(ctx context.Context, p string) (string, error) {
	return wslpath(ctx, "-u", p)
}

func wslpath(ctx context.Context, mode, p string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wslProbeTimeout)
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

// windowsCommand finds a Windows program to run from this side. It is normally
// on PATH, since WSL appends the Windows one to it — but a distribution
// configured with appendWindowsPath=false has none of that PATH and the program
// is still there. Its Windows path is known; where the drive holding it is
// mounted is not, and wslpath knows.
func windowsCommand(ctx context.Context, name, windowsPath string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return wslLinuxPath(ctx, windowsPath)
}

// wslWindowsFolder asks Windows for one of its own folder variables and returns
// it in both spellings.
//
// It is asked through cmd.exe rather than read from this process's environment:
// the Windows environment is not inherited into a WSL shell, and %LOCALAPPDATA%
// and %USERPROFILE% are the two things a config written for the Windows side
// has to get right. The stderr cmd.exe writes when it dislikes the working
// directory — this process's, which is not a drive path — is captured and
// dropped: it warns, then answers anyway.
func wslWindowsFolder(ctx context.Context, name string) (sshPath, error) {
	ctx, cancel := context.WithTimeout(ctx, wslProbeTimeout)
	defer cancel()
	interpreter, err := windowsCommand(ctx, "cmd.exe", `C:\Windows\System32\cmd.exe`)
	if err != nil {
		return sshPath{}, err
	}
	//nolint:gosec // G204: name is one of this file's own constants, not input.
	out, err := exec.CommandContext(ctx, interpreter, "/c", "echo %"+name+"%").Output()
	if err != nil {
		return sshPath{}, fmt.Errorf("ask Windows for %%%s%%: %w", name, err)
	}
	value := strings.TrimSpace(string(out))
	// cmd.exe echoes an unset variable back as its own name in percent signs.
	if value == "" || strings.Contains(value, "%") {
		return sshPath{}, fmt.Errorf("no %%%s%% in the Windows environment", name)
	}
	local, err := wslLinuxPath(ctx, value)
	if err != nil {
		return sshPath{}, err
	}
	return sshPath{local: local, client: value}, nil
}
