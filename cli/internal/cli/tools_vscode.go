package cli

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// vscodeEditorEnv names the editor binary without repeating --editor on every
// run, for a machine whose editor is not one of the builds looked for.
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

// vscodeFamily is the VS Code builds this command knows how to launch, in the
// order it looks for them. They are all the same program with the same CLI, so
// the only question is which one is installed; --editor names one directly when
// more than one is, or when it is something else entirely.
var vscodeFamily = editorFamily{
	label:      "VS Code",
	env:        vscodeEditorEnv,
	candidates: []string{"code", "code-insiders", "codium", "cursor", "windsurf"},
	launchEnv:  []string{vscodeQuietWSLPrompt},
}

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
	cmd.Flags().StringVar(&editor, "editor", "", vscodeFamily.editorFlagHelp())
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
	editor, err := vscodeFamily.resolve(opts.editor)
	if err != nil {
		return err
	}
	// The user typed this command, so what it does on their behalf is printed
	// where its own reporting goes. Driven from the launcher's window instead,
	// that stream is io.Discard and none of it reaches the screen — see
	// apiDataSource.OpenEditor.
	notes := printedNotes(cmd.ErrOrStderr())
	targets, err := vscodeFamily.sshTargets(cmd.Context(), editor, notes)
	if err != nil {
		return err
	}

	remote, editorArgs, err := a.editorRemote(cmd, targets, opts.sandboxArg, opts.source, opts.args, notes)
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
	return vscodeFamily.launch(cmd, editor, full)
}

// vscodeFolderURI is the folder as VS Code's own remote URI: the authority
// names the Remote-SSH host, the path the directory in it. Built rather than
// pasted together so a workdir with a space or a percent sign in it survives
// being one.
func vscodeFolderURI(host, folder string) string {
	uri := url.URL{Scheme: "vscode-remote", Host: "ssh-remote+" + host, Path: folder}
	return uri.String()
}
