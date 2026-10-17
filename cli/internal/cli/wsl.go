package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/wsl"
)

// WSL is the one place where the program this CLI hands work to does not run on
// the same side of the machine as the CLI. A discobox reached from a Linux
// distribution is opened by a Windows editor, connected to by Windows OpenSSH,
// and configured by files under a Windows profile — none of which can be
// spelled, quoted or executed the way this side spells, quotes and executes
// them. What that costs the emitted ssh_config is in sshTarget; asking Windows
// the questions only Windows can answer is internal/wsl's, and what this
// package builds on those answers is here.

// wslDistroEnv is set in every WSL shell, and names the distribution `wsl.exe
// -d` re-enters.
const wslDistroEnv = "WSL_DISTRO_NAME"

// isWindowsExecutable reports whether path names a file Windows can run: one on
// a mounted Windows drive rather than in this distribution's own filesystem.
//
// wslpath answers it rather than a /mnt prefix test, because where the drives
// are mounted is configurable (/etc/wsl.conf `root`) — and because the answer
// for a path inside the distribution is unmistakable: it comes back as a
// \\wsl.localhost UNC share, which is exactly the thing Windows can reach but
// should not be asked to.
func isWindowsExecutable(ctx context.Context, path string) bool {
	windows, err := wsl.WindowsPath(ctx, path)
	return err == nil && !strings.HasPrefix(windows, `\\`)
}

// wslWindowsFolder is a Windows folder variable in both spellings.
func wslWindowsFolder(ctx context.Context, name string) (sshPath, error) {
	value, err := wsl.Env(ctx, name)
	if err != nil {
		return sshPath{}, err
	}
	local, err := wsl.LinuxPath(ctx, value)
	if err != nil {
		return sshPath{}, err
	}
	return sshPath{local: local, client: value}, nil
}

// windowsTools is what a call across the boundary needs Windows to have already
// answered: where icacls is, and what this user is called over there. Both are
// constants for the life of the process and each costs a Windows process launch
// to learn, so they are asked once — with the folders that come from the same
// two programs, when the Windows target is resolved — and carried on the target
// rather than re-asked per file. A run narrows four files, and a Windows spawn
// under interop is not free.
type windowsTools struct {
	icacls string
	user   string
}

// resolveWindowsTools asks Windows both questions. It is the Windows target's
// to ask: a machine where interop cannot answer has no Windows side to write
// for, and that is the failure windowsSSHTarget already reports (ADR 0102 §3).
func resolveWindowsTools(ctx context.Context) (windowsTools, error) {
	icacls, err := wsl.Command(ctx, "icacls.exe", `C:\Windows\System32\icacls.exe`)
	if err != nil {
		return windowsTools{}, err
	}
	user, err := wsl.Env(ctx, "USERNAME")
	if err != nil {
		return windowsTools{}, err
	}
	return windowsTools{icacls: icacls, user: user}, nil
}

// restrictFile narrows a file written for the Windows side to the
// principals ssh.exe will read one under, and checks that it worked.
//
// This is privatefs.Restrict's job from the other side of the boundary: a mode bit
// written from here means nothing to Windows, and ssh.exe refuses to read a
// private key — or a config, or a known_hosts — that a principal it does not
// trust can reach. Neither default is safe to inherit — WSL puts an explicit
// S-1-5-32 ACE on everything it creates on a drive mount, and a Windows profile
// hands its own groups read access downward — so both are removed and the ACL
// is granted outright. Full control rather than read: this CLI rewrites these
// files on every run.
//
// Every file this CLI puts on that side, not only the key. ssh checks the
// user's ssh_config and every file that config Includes by the same rule it
// checks a key by, and refuses the *whole* config over any one of them: a
// managed config left under a wide ACL takes every host in that file down with
// it, this CLI's stanzas and the user's own hosts alike.
//
// The three it grants are the three privatefs.Restrict grants natively, which are
// the three Windows OpenSSH accepts: the user, SYSTEM, and Administrators. The
// last two are on every file a Windows profile holds, ssh reads those files,
// and removing them would take away the user's own recovery path without taking
// anything away from anybody else. See ADR 0102.
//
// The result is read back rather than assumed. icacls reports success for a
// grant that leaves another principal's ACE in place, which is exactly the
// failure this exists to prevent, and a file ssh will not read is worth an
// error naming what can reach it rather than OpenSSH's "Bad owner or
// permissions", which names neither the principal nor the file it read.
//
// What the read-back catches it does not repair. `/grant:r` replaces the grants
// of the three principals named and nothing else, so an explicit ACE for a
// fourth survives `/inheritance:r` and is reported rather than removed (ADR
// 0102 §2).
//
// The owner it leaves alone, which privatefs.Restrict does not. This side did not
// create most of these files — the user's own ssh_config least of all — and the
// owner a drive mount hands out is one ssh accepts already: the user, or
// Administrators when the distribution was started elevated. Taking ownership
// of a file from here needs a right over it the user may not hold, and failing
// over that would drop the Windows side of a machine whose files ssh is
// perfectly happy with. An owner ssh does reject is left for ssh to say so.
func (w windowsTools) restrictFile(ctx context.Context, windowsPath string) error {
	ctx, cancel := context.WithTimeout(ctx, wsl.ProbeTimeout)
	defer cancel()
	icacls, user := w.icacls, w.user
	granted := []string{user, localSystemSID, administratorsSID}
	args := []string{windowsPath, "/inheritance:r", "/remove:g", builtinDomainSID}
	for _, principal := range granted {
		args = append(args, "/grant:r", principal+":F")
	}
	//nolint:gosec // G204: a path this command wrote and a name Windows gave it.
	if out, err := exec.CommandContext(ctx, icacls, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("restrict %s to %s: %w: %s", windowsPath, user, err, strings.TrimSpace(string(out)))
	}
	//nolint:gosec // G204: as above.
	out, err := exec.CommandContext(ctx, icacls, windowsPath).Output()
	if err != nil {
		return fmt.Errorf("read the ACL of %s: %w", windowsPath, err)
	}
	// Counted rather than named, because only one of the three prints under a
	// name this side knows: SYSTEM and Administrators are localized, and the
	// SIDs they were granted by are not what icacls prints back. Three ACEs
	// with the user's among them is the grant above and nothing else; anything
	// more is an explicit ACE that survived it, which is the case this reads
	// back to catch.
	principals := aclPrincipals(string(out), windowsPath)
	if len(principals) != len(granted) || !aclNamesUser(principals, user) {
		return fmt.Errorf("%s is readable by %s, and ssh reads a key or a config only when nothing "+
			"but %s, SYSTEM and Administrators can; grant it to those three alone",
			windowsPath, strings.Join(principals, ", "), user)
	}
	return nil
}

// builtinDomainSID is the ACE WSL adds to every file it creates on a mounted
// Windows drive. It is named by SID rather than by name because the name it
// resolves to is neither stable nor localized the same way everywhere; icacls
// prints it as BUILTIN\BUILTIN.
const builtinDomainSID = "*S-1-5-32"

// The two principals a private key may carry besides its owner, by SID for the
// same reason: BUILTIN\Administrators and NT AUTHORITY\SYSTEM are spelled in
// the display language of the machine, and a grant by name would be a grant to
// nobody on half of them.
const (
	localSystemSID    = "*S-1-5-18"
	administratorsSID = "*S-1-5-32-544"
)

// aclPrincipals lists the principals icacls reports on one file. Each ACE is
// `PRINCIPAL:(rights)`, the first sharing a line with the path; the count line
// icacls ends with carries no such entry and is skipped.
func aclPrincipals(listing, windowsPath string) []string {
	var principals []string
	for _, line := range strings.Split(listing, "\n") {
		entry := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), windowsPath))
		if rights := strings.Index(entry, ":("); rights > 0 {
			principals = append(principals, entry[:rights])
		}
	}
	return principals
}

// aclNamesUser reports whether one of the principals is this user. A principal
// is `DOMAIN\name`, and it is the name that identifies the user.
func aclNamesUser(principals []string, user string) bool {
	for _, principal := range principals {
		if name := principal[strings.LastIndex(principal, `\`)+1:]; strings.EqualFold(name, user) {
			return true
		}
	}
	return false
}
