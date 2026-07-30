package execs

import (
	"bufio"
	"fmt"
	"os"
	osuser "os/user"
	"strings"
)

// passwdPath is the OS user database ResolveShell reads. It is a variable so
// tests can point it at a fixture instead of the host's real database.
var passwdPath = "/etc/passwd"

// fallbackShell is the last resort when nothing else resolves: every Unix has
// it, so a shell exec never fails for want of a shell.
const fallbackShell = "/bin/sh"

// ResolveShell reports the login shell of the user an exec runs as. The user's
// passwd entry is the authority — "the user's shell" is a property of the
// system the process runs on, not of the client asking for one — with $SHELL
// from the exec environment and then a probe of the usual paths behind it, for
// a run user that has no passwd entry (a bare UID) or whose entry names a
// login-refusing shell such as /usr/sbin/nologin.
//
// An empty user means the exec inherits the agent's own identity, so the
// current process user is looked up instead.
func ResolveShell(user *User, env map[string]string) (string, error) {
	name := ""
	if user != nil {
		name = strings.TrimSpace(user.Name)
	}
	if name == "" {
		if emptyUser(user) {
			if current, err := osuser.Current(); err == nil {
				name = strings.TrimSpace(current.Username)
			}
		} else {
			// A run user given only as a UID still has a login shell whenever the
			// OS database knows the UID.
			resolved, _, err := ResolveUser(user)
			if err != nil {
				return "", err
			}
			name = resolved
		}
	}
	if name != "" {
		shell, err := passwdShell(name)
		if err != nil {
			return "", err
		}
		if isLoginShell(shell) {
			return shell, nil
		}
	}
	if shell := strings.TrimSpace(env["SHELL"]); isLoginShell(shell) {
		return shell, nil
	}
	for _, candidate := range []string{"/bin/bash", "/bin/sh"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return fallbackShell, nil
}

// ShellCommand is the argv for an interactive login shell for user: a login
// shell so the exec sees the same profile-sourced environment a real login
// session would.
func ShellCommand(user *User, env map[string]string) ([]string, error) {
	shell, err := ResolveShell(user, env)
	if err != nil {
		return nil, err
	}
	return []string{shell, "-l"}, nil
}

// passwdShell reads the shell field of name's passwd entry. os/user does not
// expose it, so the database is parsed directly. A missing entry is not an
// error: the caller falls back, since a user can run a process without one.
func passwdShell(name string) (string, error) {
	file, err := os.Open(passwdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", passwdPath, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		return strings.TrimSpace(fields[6]), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", passwdPath, err)
	}
	return "", nil
}

// isLoginShell rejects the shells a system uses to refuse interactive logins.
// Running one hands the caller an immediate exit instead of a session, so it is
// treated as "no shell configured" and the next fallback is used.
func isLoginShell(shell string) bool {
	shell = strings.TrimSpace(shell)
	if shell == "" {
		return false
	}
	switch {
	case strings.HasSuffix(shell, "/nologin"), strings.HasSuffix(shell, "/false"):
		return false
	}
	return true
}
