package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/tools"
)

// Running a tool is one of two things (ADR 0125 §3): an exec in the discobox,
// or a program on this machine handed the discobox. Both are here, once, for
// `discobox tools <id>` and for the launcher's picker alike.

// sandboxToolCommand is the argv a sandbox tool runs as, with the caller's
// arguments after the declaration's.
//
// A script out of the discobox is run by its path there. One from this machine
// is not in the discobox at all, so it rides the exec itself: written into a
// directory of its own and run from there, in one step (deliverToolScript).
func sandboxToolCommand(def tools.Definition, args []string) []string {
	var command []string
	switch {
	case !def.Script:
		command = []string{def.Program[0]}
	case def.Layer.InSandbox():
		command = []string{def.Path}
	default:
		command = []string{"sh", "-c", deliverToolScript, "sh", def.FileName, string(def.ScriptData())}
	}
	command = append(command, def.Args...)
	return append(command, args...)
}

// deliverToolScript writes $2 into a directory of its own as $1, makes it
// executable, runs it with the rest of the arguments, and removes it again.
//
// A directory made for this run and nowhere shared: the run user's cache is a
// pool-shared volume, where another discobox as the same user could replace the
// file between the write and the run, and two scripts with one file name would
// overwrite each other. /tmp is the container's own. The script runs as a child
// rather than by exec so the directory can go when it ends. The traps are what
// keep the shell there to remove it: INT and TERM reach the tool as well, so
// the shell only has to outlive it (a handler, unlike an ignore, is not
// inherited by the tool) and exit with the tool's own status, which also
// leaves a tool that catches Ctrl-C and carries on reporting how it ended. A
// hang-up means the terminal is gone and the shell leaves with it.
const deliverToolScript = `set -eu
dir=$(mktemp -d "${TMPDIR:-/tmp}/discobox-tool.XXXXXX")
trap 'rm -rf "$dir"' EXIT
trap 'exit 129' HUP
trap : INT TERM
tool="$dir/$1"
printf %s "$2" > "$tool"
chmod 700 "$tool"
shift 2
"$tool" "$@"`

// hostToolLaunch is a host tool resolved far enough to run: what to exec, with
// which arguments, before the discobox has been handed to it.
type hostToolLaunch struct {
	def tools.Definition
	// program is the resolved binary, or the script's interpreter.
	program string
	// prefix comes before the declaration's args: an interpreter's own flags
	// and the script's path.
	prefix []string
	// windowsProgram is whether the program is a Windows executable run from
	// WSL, which is what decides that the Windows ssh_config is required.
	windowsProgram bool
}

// resolveHostTool finds what a host tool will run, before anything is written
// for it: a missing program is the one failure nothing can fix afterwards, and
// refreshing an ssh_config for a window that will never open is work nobody
// asked for.
func resolveHostTool(ctx context.Context, def tools.Definition, named string) (hostToolLaunch, error) {
	launch := hostToolLaunch{def: def}
	switch {
	case !def.Script:
		program, err := def.ResolveProgram(named)
		if err != nil {
			return hostToolLaunch{}, err
		}
		launch.program = program
		launch.windowsProgram = isWSL() && isWindowsExecutable(ctx, program)
	case named != "":
		return hostToolLaunch{}, fmt.Errorf("tool %s is a script, and --program names the program a .yaml tool runs", def.ID)
	case strings.EqualFold(filepath.Ext(def.Path), ".ps1"):
		shell, err := powerShell()
		if err != nil {
			return hostToolLaunch{}, err
		}
		launch.program = shell
		launch.prefix = []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", def.Path}
	default:
		launch.program = def.Path
	}
	return launch, nil
}

// powerShell is the PowerShell a .ps1 tool runs under: PowerShell 7 where it is
// installed, and Windows PowerShell where it is not.
func powerShell() (string, error) {
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("a .ps1 tool runs under PowerShell, and neither pwsh nor powershell is on PATH")
}

// sshTargets is every ssh installation on this machine the refreshed config has
// to reach, which on WSL is two.
//
// Which one the tool will drive decides whether a missing one is fatal: a
// Windows build launched from WSL connects with Windows OpenSSH, and without
// that config there is nothing for it to connect to. Anywhere else the Windows
// side is worth a note and no more. Not resolving it is only the first way that
// side can fail, and the rest are decided the same way: mirroring the key,
// setting its ACL and writing the files are all allowed to fail with a note
// (ADR 0102 §3), so a Windows program clears `optional` and gets an error
// wherever the failure happens instead.
func (l hostToolLaunch) sshTargets(ctx context.Context, notes noteFunc) ([]sshTarget, error) {
	targets, err := machineSSHTargets(ctx)
	if err != nil {
		return nil, err
	}
	if targets.windowsErr != nil {
		if l.windowsProgram {
			hint := "name a Linux build with --program"
			if l.def.ProgramEnv != "" {
				hint += " or $" + l.def.ProgramEnv
			}
			return nil, fmt.Errorf("%s is a Windows program, so it connects with Windows OpenSSH: %w; %s to use this machine's own ssh_config instead",
				l.program, targets.windowsErr, hint)
		}
		notes(windowsSSHConfigSkipped, targets.windowsErr)
	}
	if l.windowsProgram {
		for i := range targets.all {
			targets.all[i].optional = false
		}
	}
	return targets.all, nil
}

// run execs the tool with the discobox handed to it, wired to the given
// streams.
func (l hostToolLaunch) run(ctx context.Context, remote tools.Remote, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	expanded, err := remote.Expand(l.def.Args)
	if err != nil {
		return err
	}
	full := append(append(append([]string{}, l.prefix...), expanded...), args...)
	//nolint:gosec // G204: a tool declared by this CLI or by the user, with the user's own arguments.
	session := exec.CommandContext(ctx, l.program, full...)
	session.Env = append(append(os.Environ(), remote.Environ()...), l.def.Env...)
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	if err := session.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("%s exited %d", l.program, exitErr.ExitCode())
		}
		return fmt.Errorf("run %s: %w", l.program, err)
	}
	return nil
}

// runHostTool hands one discobox to a host tool: the program is found, the
// project's managed ssh_config refreshed for every ssh it might use, and the
// tool run with the discobox's host and working tree.
func (a *App) runHostTool(ctx context.Context, def tools.Definition, target toolTarget, named string, args []string, stdin io.Reader, stdout, stderr io.Writer, notes noteFunc) error {
	launch, err := resolveHostTool(ctx, def, named)
	if err != nil {
		return err
	}
	targets, err := launch.sshTargets(ctx, notes)
	if err != nil {
		return err
	}
	remote, err := target.app.sandboxSSHRemote(ctx, targets, target.client, target.projectID, target.sandboxID, target.source, notes)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "opening %s in %s\n", remote.describe(), launch.program)
	return launch.run(ctx, tools.Remote{SandboxID: target.sandboxID, Host: remote.host, Workdir: remote.folder}, args, stdin, stdout, stderr)
}

// toolTarget is the discobox a tool was pointed at, on the server it is on.
type toolTarget struct {
	app       *App
	client    *apiclientgen.Client
	projectID string
	sandboxID string
	// source is the source slug to work in, empty for the primary.
	source string
}

// resolveToolTarget is the discobox a `discobox tools` command names, and the
// arguments left over for the tool.
//
// The discobox is --discobox-id when that was given, and otherwise whatever
// `discobox shell` would have resolved from this directory — a leading
// argument naming one of its discoboxes, or a pick — which is also what decides
// where the tool's own arguments begin.
func (a *App) resolveToolTarget(cmd *cobra.Command, sandboxArg, source string, args []string) (toolTarget, []string, error) {
	target := toolTarget{source: source}
	var err error
	rest := args
	if strings.TrimSpace(sandboxArg) != "" {
		target.app, target.projectID, target.sandboxID, target.client, err = a.selectSandbox(cmd, sandboxArg)
	} else {
		target.app, target.projectID, target.sandboxID, target.client, rest, err = a.resolveShellTarget(cmd, args)
	}
	return target, rest, err
}
