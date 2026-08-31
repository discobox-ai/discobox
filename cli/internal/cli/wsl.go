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

// windowsEnv asks Windows for one of its own environment variables.
//
// It is asked through cmd.exe rather than read from this process's environment,
// because the Windows environment is not inherited into a WSL shell — and
// %LOCALAPPDATA%, %USERPROFILE% and %USERNAME% are what a config written for
// the Windows side has to get right. The stderr cmd.exe writes when it dislikes
// the working directory — this process's, which is not a drive path — is
// captured and dropped: it warns, then answers anyway.
func windowsEnv(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wslProbeTimeout)
	defer cancel()
	interpreter, err := windowsCommand(ctx, "cmd.exe", `C:\Windows\System32\cmd.exe`)
	if err != nil {
		return "", err
	}
	//nolint:gosec // G204: name is one of this file's own constants, not input.
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

// wslWindowsFolder is a Windows folder variable in both spellings.
func wslWindowsFolder(ctx context.Context, name string) (sshPath, error) {
	value, err := windowsEnv(ctx, name)
	if err != nil {
		return sshPath{}, err
	}
	local, err := wslLinuxPath(ctx, value)
	if err != nil {
		return sshPath{}, err
	}
	return sshPath{local: local, client: value}, nil
}

// restrictWindowsKey narrows a mirrored private key to this user alone, and
// checks that it worked.
//
// This is restrictToUser's job from the other side of the boundary: a mode bit
// written from here means nothing to Windows, and ssh.exe refuses to read a
// private key any other principal can. Neither default is safe to inherit —
// WSL puts an explicit S-1-5-32 ACE on everything it creates on a drive mount,
// and a Windows profile hands its own groups read access downward — so both are
// removed and the user is granted the file outright. Full control rather than
// read: this CLI rewrites the key on every run.
//
// The result is read back rather than assumed. icacls reports success for a
// grant that leaves another principal's ACE in place, which is exactly the
// failure this exists to prevent, and a key ssh will not read is worth an error
// here rather than "Permissions for … are too open" from a program the user did
// not run themselves.
func restrictWindowsKey(ctx context.Context, windowsPath string) error {
	ctx, cancel := context.WithTimeout(ctx, wslProbeTimeout)
	defer cancel()
	icacls, err := windowsCommand(ctx, "icacls.exe", `C:\Windows\System32\icacls.exe`)
	if err != nil {
		return err
	}
	user, err := windowsEnv(ctx, "USERNAME")
	if err != nil {
		return err
	}
	//nolint:gosec // G204: a path this command wrote and a name Windows gave it.
	set := exec.CommandContext(ctx, icacls, windowsPath,
		"/inheritance:r", "/remove:g", builtinDomainSID, "/grant:r", user+":F")
	if out, err := set.CombinedOutput(); err != nil {
		return fmt.Errorf("restrict %s to %s: %w: %s", windowsPath, user, err, strings.TrimSpace(string(out)))
	}
	//nolint:gosec // G204: as above.
	out, err := exec.CommandContext(ctx, icacls, windowsPath).Output()
	if err != nil {
		return fmt.Errorf("read the ACL of %s: %w", windowsPath, err)
	}
	if others := aclPrincipalsBesides(string(out), windowsPath, user); len(others) > 0 {
		return fmt.Errorf("%s is readable by %s, and ssh will not read a private key that anyone else can; "+
			"grant it to %s alone", windowsPath, strings.Join(others, ", "), user)
	}
	return nil
}

// builtinDomainSID is the ACE WSL adds to every file it creates on a mounted
// Windows drive. It is named by SID rather than by name because the name it
// resolves to is neither stable nor localized the same way everywhere; icacls
// prints it as BUILTIN\BUILTIN.
const builtinDomainSID = "*S-1-5-32"

// aclPrincipalsBesides lists the principals in icacls output that are not the
// one expected. Each ACE is `PRINCIPAL:(rights)`, the first sharing a line with
// the path; a principal is `DOMAIN\name`, and it is the name that identifies
// the user.
func aclPrincipalsBesides(listing, windowsPath, user string) []string {
	var others []string
	for _, line := range strings.Split(listing, "\n") {
		entry := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), windowsPath))
		rights := strings.Index(entry, ":(")
		if rights < 0 {
			continue
		}
		principal := entry[:rights]
		if name := principal[strings.LastIndex(principal, `\`)+1:]; !strings.EqualFold(name, user) {
			others = append(others, principal)
		}
	}
	return others
}
