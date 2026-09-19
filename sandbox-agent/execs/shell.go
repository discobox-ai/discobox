package execs

import (
	"os"
	"path"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
	"github.com/discobox-ai/discobox/sandboxshell"
	"github.com/discobox-ai/discobox/sandboxuser"
)

// shellLauncher is the image's `discobox-shell`: it runs the shell a client
// prefers when the sandbox has it, and the login shell it is handed otherwise.
// Finding the preferred shell is image work rather than agent work, because a
// shell the repository's flake provides is on PATH only once the login profile
// and the source tree's .envrc have both run, and only a shell can run them.
const shellLauncher = "/usr/local/bin/discobox-shell"

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

// PreferredShellCommand is the argv for an interactive login shell that honors
// the shell the client asked for through sandboxshell.PreferredEnv in the
// request's own environment (ADR 0138), and ShellCommand's argv when it asked
// for none. The login shell ShellCommand would have run is passed along as the
// launcher's fallback, so the sandbox's answer to "which shell does this user
// have" is still decided here and only here.
//
// Only the request's environment is read, never the merged one: a preference is
// what a person at a client asked for, not something an image or a manifest
// can set on every shell the sandbox starts.
func PreferredShellCommand(user *User, requestEnv, env map[string]string) ([]string, error) {
	login, err := ShellCommand(user, env)
	if err != nil {
		return nil, err
	}
	preferred := strings.TrimSpace(requestEnv[sandboxshell.PreferredEnv])
	// A preference for the login shell itself -- the same path, or the same
	// name, which is what a Mac's /bin/bash or a brew bash means here -- is no
	// preference, and runs as the login shell always has, unwrapped: the record
	// then names the shell that ran rather than the launcher.
	if preferred == "" || preferred == login[0] || path.Base(preferred) == path.Base(login[0]) {
		return login, nil
	}
	return append([]string{shellLauncher, preferred}, login...), nil
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
		b.WriteString(QuoteShellArg(arg))
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

// QuoteShellArg renders one argument so a POSIX shell reads it as the single
// literal word it is. Single quotes are the only form that needs no knowledge
// of what is inside them; the closing/escape/reopening dance is how a single
// quote itself gets through.
func QuoteShellArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
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
