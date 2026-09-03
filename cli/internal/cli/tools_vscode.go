package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// vscodeEditors are the VS Code builds this command knows how to launch, in the
// order it looks for them. They are all the same program with the same CLI, so
// the only question is which one is installed; --editor names one directly when
// more than one is, or when it is something else entirely.
var vscodeEditors = []string{"code", "code-insiders", "codium", "cursor", "windsurf"}

// vscodeEditorEnv names the editor binary without repeating --editor on every
// run, for a machine whose editor is not one of the names above.
const vscodeEditorEnv = "DISCOBOX_VSCODE"

// vscodeQuietWSLPrompt silences the question VS Code's launcher asks when it
// finds itself installed inside WSL — "please install VS Code in Windows
// instead… Continue anyway? [y/N]".
//
// It is set unconditionally rather than only under WSL, because it does nothing
// anywhere else and a conditional would be a second thing to get wrong. The
// prompt is a warning to someone typing `code`, and this is not that: the
// editor is being launched by a command that has already decided which binary
// to run and, when that binary is the Windows one, has already written the
// config the Windows side needs to connect. Left alone, the prompt reads from a
// stdin nobody is typing at and the command hangs or aborts on the default No.
const vscodeQuietWSLPrompt = "DONT_PROMPT_WSL_INSTALL=1"

func (a *App) newToolsVSCodeCommand(sandboxID *string) *cobra.Command {
	var source string
	var editor string
	var reuseWindow bool
	cmd := &cobra.Command{
		Use:     "vscode [DISCOBOX_ID] [-- EDITOR_ARG...]",
		Aliases: []string{"code"},
		Short:   "Open a discobox in VS Code over Remote-SSH",
		Long: `Open a discobox in VS Code, editing it in place over Remote-SSH.

This refreshes the ssh_config this CLI manages for the project — the same one
` + "`discobox admin ssh-config --write`" + ` writes — and then opens the discobox's working
tree in a new VS Code window pointed at it. The stanzas reach the server through
this CLI rather than an address, so the server needs no SSH port and this
command holds nothing open: once VS Code has the host, it connects on its own
and reconnects on its own.

Without --source the window opens on the discobox's primary source.

Arguments are passed to the editor untouched; only the flags before them are
consumed here. Use -- when an editor argument would otherwise be read as one of
them.`,
		Example: `  discobox tools vscode
  discobox tools vscode mybox
  discobox tools vscode -s docs
  discobox tools vscode --editor cursor`,
		// Stop parsing at the first positional argument so a leading sandbox
		// reference and anything meant for the editor reach us intact.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runToolsVSCode(cmd, toolsVSCodeOptions{
				sandboxArg:  *sandboxID,
				source:      source,
				editor:      editor,
				reuseWindow: reuseWindow,
				args:        args,
			})
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVarP(&source, "source", "s", "", "Source to open, named by its slug; defaults to the discobox's primary source")
	cmd.Flags().StringVar(&editor, "editor", "", "Editor binary to run (default: $"+vscodeEditorEnv+", or the first of "+strings.Join(vscodeEditors, ", ")+" on PATH)")
	cmd.Flags().BoolVar(&reuseWindow, "reuse-window", false, "Open in the current VS Code window instead of a new one")
	return cmd
}

type toolsVSCodeOptions struct {
	sandboxArg  string
	source      string
	editor      string
	reuseWindow bool
	args        []string
}

func (a *App) runToolsVSCode(cmd *cobra.Command, opts toolsVSCodeOptions) error {
	// Which editor to run is resolved before anything is written: it is the one
	// failure the user can do nothing about afterwards, and refreshing an
	// ssh_config for a window that will never open is work nobody asked for.
	editor, err := resolveVSCodeEditor(opts.editor)
	if err != nil {
		return err
	}
	// Every ssh on this machine gets the refreshed stanzas, but which one the
	// editor will drive decides whether a missing one is fatal: a Windows
	// build launched from WSL connects with Windows OpenSSH, and without that
	// config there is nothing for it to connect to.
	// The user typed this command, so what it does on their behalf is printed
	// where its own reporting goes. Driven from the launcher's window instead,
	// that stream is io.Discard and none of it reaches the screen — see
	// apiDataSource.OpenEditor.
	notes := printedNotes(cmd.ErrOrStderr())
	targets, windowsErr := machineSSHTargets(cmd.Context())
	if windowsErr != nil {
		if isWindowsExecutable(cmd.Context(), editor) {
			return fmt.Errorf("%s is a Windows program, so it connects with Windows OpenSSH: %w; "+
				"name a Linux build with --editor or $%s to use this machine's own ssh_config instead",
				editor, windowsErr, vscodeEditorEnv)
		}
		notes("not writing the Windows ssh_config: %v", windowsErr)
	}

	var projectID, sandboxID string
	var client *apiclientgen.Client
	var editorArgs []string
	if strings.TrimSpace(opts.sandboxArg) != "" {
		projectID, sandboxID, client, err = a.selectSandbox(cmd, opts.sandboxArg)
		editorArgs = opts.args
	} else {
		projectID, sandboxID, client, editorArgs, err = a.resolveShellTarget(cmd, opts.args)
	}
	if err != nil {
		return err
	}

	remote, err := a.vscodeRemoteTarget(cmd.Context(), targets, client, projectID, sandboxID, opts.source, notes)
	if err != nil {
		return err
	}

	var full []string
	if opts.reuseWindow {
		full = append(full, "--reuse-window")
	} else {
		// A new window by default: the one you are reading this in is on
		// something else, and Remote-SSH would take it over.
		full = append(full, "--new-window")
	}
	if remote.folder != "" {
		// A URI rather than --remote and a path, because a path argument is
		// the one thing the launcher rewrites. VS Code started from WSL is
		// usually the Windows build, whose CLI reads a bare path as a path in
		// *this* distribution: it translates it into a wsl+<distro> remote and
		// opens the local directory instead of the discobox. A folder URI
		// carries its own authority and is passed through untouched.
		full = append(full, "--folder-uri", vscodeFolderURI(remote.host, remote.folder))
	} else {
		full = append(full, "--remote", "ssh-remote+"+remote.host)
	}
	full = append(full, editorArgs...)

	fmt.Fprintf(cmd.ErrOrStderr(), "opening %s in %s\n", remote.describe(), editor)
	session := exec.CommandContext(cmd.Context(), editor, full...) //nolint:gosec // G204: this command's own arguments, plus the user's own editor arguments.
	session.Env = append(os.Environ(), vscodeQuietWSLPrompt)
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

// vscodeRemoteTarget is where the editor is pointed: the ssh host that reaches
// the sandbox, and the directory in it to open.
type vscodeRemoteTarget struct {
	host   string
	folder string
}

func (t vscodeRemoteTarget) describe() string {
	if t.folder == "" {
		return t.host
	}
	return t.host + ":" + t.folder
}

// vscodeRemoteTarget refreshes the project's managed ssh_config and works out
// what to open in it.
//
// The config is written rather than printed because Remote-SSH reads
// ssh_config and nothing else: it drives the system `ssh` binary, so the only
// way to hand it a host is to put the host where ssh finds it. Writing the
// whole project's stanzas rather than this sandbox's is what `ssh-config
// --write` already means by that file — it is rewritten wholesale on every run
// — and it leaves every other sandbox reachable too.
func (a *App) vscodeRemoteTarget(ctx context.Context, targets []sshTarget, client *apiclientgen.Client, projectID, sandboxID, sourceSlug string, notes noteFunc) (vscodeRemoteTarget, error) {
	resolvedProjectID, err := a.concreteProjectID(ctx, client, projectID)
	if err != nil {
		return vscodeRemoteTarget{}, err
	}
	hostKey, err := a.sshHostKey(ctx, client)
	if err != nil {
		return vscodeRemoteTarget{}, err
	}
	built, err := a.buildManagedSSHConfig(ctx, managedSSHConfigRequest{
		client:            client,
		projectID:         projectID,
		resolvedProjectID: resolvedProjectID,
		hostKey:           hostKey,
		write:             true,
		notes:             notes,
	}, targets)
	if err != nil {
		return vscodeRemoteTarget{}, err
	}
	for _, config := range built {
		if err := writeManagedSSHConfig(config, resolvedProjectID, notes); err != nil {
			return vscodeRemoteTarget{}, err
		}
	}

	host, ok := built[0].aliases[sandboxID]
	if !ok {
		// Every spelling of this sandbox was claimed by another one, so the
		// config carries no stanza it could be reached by. See
		// sshConfigHostPatterns.
		return vscodeRemoteTarget{}, fmt.Errorf("discobox %s has no unambiguous SSH host alias; rename it or the discobox whose name spells its ID", sandboxID)
	}
	folder, err := a.vscodeFolder(ctx, client, projectID, sandboxID, sourceSlug)
	if err != nil {
		return vscodeRemoteTarget{}, err
	}
	return vscodeRemoteTarget{host: host, folder: folder}, nil
}

// vscodeFolder is the directory in the sandbox the window opens on.
//
// An SSH session lands in the run user's home directory rather than the
// sandbox's exec default, so unlike `discobox tools git` this cannot leave the
// directory unsaid: without one, VS Code would open a window on the home
// directory and the working tree would be somewhere else. Empty is still
// possible — a sandbox may not have told us where its source landed — and then
// the window opens on the host with no folder, which is VS Code's own way of
// saying "connected, nothing open".
func (a *App) vscodeFolder(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, sourceSlug string) (string, error) {
	if sourceSlug != "" {
		return a.toolSourceWorkdir(ctx, client, projectID, sandboxID, sourceSlug)
	}
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return "", err
	}
	sources := applySources(sandbox)
	if len(sources) == 0 {
		return "", nil
	}
	return sourceWorkdir(sources[0].source), nil
}

// vscodeFolderURI is the folder as VS Code's own remote URI: the authority
// names the Remote-SSH host, the path the directory in it. Built rather than
// pasted together so a workdir with a space or a percent sign in it survives
// being one.
func vscodeFolderURI(host, folder string) string {
	uri := url.URL{Scheme: "vscode-remote", Host: "ssh-remote+" + host, Path: folder}
	return uri.String()
}

// resolveVSCodeEditor finds the editor binary to run: what was named, or the
// first VS Code build on PATH.
func resolveVSCodeEditor(named string) (string, error) {
	if strings.TrimSpace(named) == "" {
		named = strings.TrimSpace(os.Getenv(vscodeEditorEnv))
	}
	if named != "" {
		path, err := exec.LookPath(named)
		if err != nil {
			return "", fmt.Errorf("%s is not installed, or not on PATH: %w", named, err)
		}
		return path, nil
	}
	for _, candidate := range vscodeEditors {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no VS Code command found on PATH (looked for %s); "+
		"install VS Code's shell command, or name yours with --editor or $%s",
		strings.Join(vscodeEditors, ", "), vscodeEditorEnv)
}
