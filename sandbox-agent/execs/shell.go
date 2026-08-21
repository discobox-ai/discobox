package execs

import (
	"os"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
	"github.com/discobox-ai/discobox/sandboxuser"
)

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
	if name == "" && !sandboxuser.Named(user) {
		// The exec inherits the agent's own identity, so the shell to resolve is
		// that account's. Who this process is belongs to runuser, which owns the
		// image layer for everyone (ADR 0033 §6).
		resolved, err := runuser.Resolve(runuser.Layers{Image: runuser.Current()}, sandboxuser.FieldName)
		if err == nil {
			name = strings.TrimSpace(resolved.Name)
		}
	}
	if name != "" {
		shell, _, err := runuser.LoginShell(name)
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

// QuoteShellCommand renders argv as a single command line safe to type into an
// interactive shell, as if a user had typed it themselves rather than passed
// it as argv: every argument is single-quoted, the one quoting form bash, zsh,
// dash, ksh, and fish all agree on (only a literal single quote needs
// escaping — close the quote, emit an escaped quote, reopen it). It ends in a
// newline so the shell executes it as soon as it is read. Feeding a command
// through the shell's normal input, rather than launching it directly, is what
// gives it real job control: see Exec.StartupCommand.
func QuoteShellCommand(argv []string) []byte {
	if len(argv) == 0 {
		return nil
	}
	var b strings.Builder
	for i, arg := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(arg, "'", `'\''`))
		b.WriteByte('\'')
	}
	b.WriteByte('\n')
	return []byte(b.String())
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
