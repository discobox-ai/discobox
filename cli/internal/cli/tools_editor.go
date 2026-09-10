package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// editorFamily is one editor this CLI can open a discobox in: the builds it
// looks for on PATH, the variable that names one it has never heard of, and
// whatever that program needs in its environment to run unattended.
//
// One type for both editors because everything either of them does around the
// launch is the same work: find the binary before anything is written, decide
// which ssh the written config has to reach, run the thing and report how it
// went. What differs is the command line — a `vscode-remote://` folder URI
// against an `ssh://` URL — and that stays in the command that means it.
type editorFamily struct {
	// label is the editor as the help and the errors name it.
	label string
	// env is the variable that names the binary, for a machine whose editor is
	// not one of the candidates.
	env string
	// candidates are the builds looked for on PATH, in order.
	candidates []string
	// launchEnv is added to the editor's environment on every run, in
	// `NAME=value` form.
	launchEnv []string
}

// editorFlagHelp is the --editor flag's help text, which has to say what the
// default actually is.
func (f editorFamily) editorFlagHelp() string {
	return "Editor binary to run (default: $" + f.env + ", or the first of " + strings.Join(f.candidates, ", ") + " on PATH)"
}

// resolve finds the editor binary to run: what was named, or the first of this
// family's builds on PATH.
func (f editorFamily) resolve(named string) (string, error) {
	if strings.TrimSpace(named) == "" {
		named = strings.TrimSpace(os.Getenv(f.env))
	}
	if named != "" {
		path, err := exec.LookPath(named)
		if err != nil {
			return "", fmt.Errorf("%s is not installed, or not on PATH: %w", named, err)
		}
		return path, nil
	}
	for _, candidate := range f.candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no %s command found on PATH (looked for %s); "+
		"install %s's shell command, or name yours with --editor or $%s",
		f.label, strings.Join(f.candidates, ", "), f.label, f.env)
}

// sshTargets is every ssh installation on this machine the refreshed config has
// to reach, which on WSL is two.
//
// Which one the editor will drive decides whether a missing one is fatal: a
// Windows build launched from WSL connects with Windows OpenSSH, and without
// that config there is nothing for it to connect to. Anywhere else the Windows
// side is worth a note and no more — this side's config is written and correct.
//
// Not resolving it is only the first way that side can fail, and the editor
// decides the rest of them the same way: mirroring the key, setting its ACL and
// writing the files are all allowed to fail with a note (ADR 0102 §3), so an
// editor that needs the Windows config clears `optional` and gets an error
// wherever the failure happens instead.
func (f editorFamily) sshTargets(ctx context.Context, editor string, notes noteFunc) ([]sshTarget, error) {
	targets, windowsErr := machineSSHTargets(ctx)
	windowsEditor := isWSL() && isWindowsExecutable(ctx, editor)
	if windowsErr != nil {
		if windowsEditor {
			return nil, fmt.Errorf("%s is a Windows program, so it connects with Windows OpenSSH: %w; "+
				"name a Linux build with --editor or $%s to use this machine's own ssh_config instead",
				editor, windowsErr, f.env)
		}
		notes(windowsSSHConfigSkipped, windowsErr)
	}
	if windowsEditor {
		for i := range targets {
			targets[i].optional = false
		}
	}
	return targets, nil
}

// launch runs the editor with the arguments the command built, wired to this
// terminal so anything it says about the connection is seen.
func (f editorFamily) launch(cmd *cobra.Command, editor string, args []string) error {
	session := exec.CommandContext(cmd.Context(), editor, args...) //nolint:gosec // G204: this command's own arguments, plus the user's own editor arguments.
	session.Env = append(os.Environ(), f.launchEnv...)
	session.Stdin, session.Stdout, session.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := session.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("%s exited %d", editor, exitErr.ExitCode())
		}
		return fmt.Errorf("run %s: %w", editor, err)
	}
	return nil
}

// editorRemote is the discobox an editor command was pointed at, as the ssh
// world reaches it, and the arguments left over for the editor itself.
//
// The discobox is the leading reference when the command was given one, and
// otherwise whatever `discobox shell` would have resolved from this directory,
// which is also what decides where the editor's own arguments begin. Resolving
// the remote refreshes the project's managed ssh_config for every target, since
// the host it names exists nowhere else.
func (a *App) editorRemote(cmd *cobra.Command, targets []sshTarget, sandboxArg, source string, args []string, notes noteFunc) (sandboxSSHRemote, []string, error) {
	var projectID, sandboxID string
	var client *apiclientgen.Client
	var editorArgs []string
	var err error
	if strings.TrimSpace(sandboxArg) != "" {
		projectID, sandboxID, client, err = a.selectSandbox(cmd, sandboxArg)
		editorArgs = args
	} else {
		projectID, sandboxID, client, editorArgs, err = a.resolveShellTarget(cmd, args)
	}
	if err != nil {
		return sandboxSSHRemote{}, nil, err
	}
	remote, err := a.sandboxSSHRemote(cmd.Context(), targets, client, projectID, sandboxID, source, notes)
	if err != nil {
		return sandboxSSHRemote{}, nil, err
	}
	return remote, editorArgs, nil
}
