package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// zedEditorEnv names the editor binary without repeating --editor on every run,
// for a machine whose Zed is not one of the builds looked for.
const zedEditorEnv = "DISCOBOX_ZED"

// zedFamily is the Zed builds this command knows how to launch, in the order it
// looks for them. A Flatpak, which is not a binary at all, is what --editor is
// for.
//
// Upstream's own installer links the CLI as `zed` on every channel, so that is
// the name almost every machine has — and it is looked for *last*, because it
// is the only ambiguous one. Packagers rename it precisely because something
// else already answers to `zed`: nixpkgs ships the data lake under that name
// and the editor as `zeditor`. On such a machine both are on PATH, and trying
// `zed` first would run the data lake on an `ssh://` URL and report "zed
// exited 1" about a program that is not an editor. Looking for the
// unambiguous names first costs nothing where only `zed` exists.
var zedFamily = editorFamily{
	label:      "Zed",
	env:        zedEditorEnv,
	candidates: []string{"zeditor", "zedit", "zed"},
}

func (a *App) newToolsZedCommand(sandboxID *string) *cobra.Command {
	var source string
	var editor string
	var reuseWindow bool
	cmd := &cobra.Command{
		Use:   "zed [DISCOBOX_ID] [-- EDITOR_ARG...]",
		Short: "Open a discobox in Zed over SSH",
		Long: `Open a discobox in Zed, editing it in place over Zed's remote development.

This refreshes the ssh_config this CLI manages for the project — the same one
` + "`discobox admin ssh-config --write`" + ` writes — and then opens the discobox's working
tree in a new Zed window pointed at it. Zed runs the ssh on this machine's PATH
and inherits that config, and the stanzas reach the server through this CLI
rather than an address, so the server needs no SSH port and this command holds
nothing open: once Zed has the host, it connects on its own.

Zed installs its own server into ~/.zed_server in the discobox on first
connection, downloading it there from zed.dev. A discobox without egress needs
"upload_binary_over_ssh": true for the host in Zed's own settings, which makes
Zed upload the binary over the SSH connection instead.

Without --source the window opens on the discobox's primary source.

Arguments are passed to the editor untouched; only the flags before them are
consumed here. Use -- when an editor argument would otherwise be read as one of
them.`,
		Example: `  discobox tools zed
  discobox tools zed mybox
  discobox tools zed -s docs
  discobox tools zed --editor zeditor`,
		// Stop parsing at the first positional argument so a leading sandbox
		// reference and anything meant for the editor reach us intact.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runToolsZed(cmd, toolsZedOptions{
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
	cmd.Flags().StringVar(&editor, "editor", "", zedFamily.editorFlagHelp())
	cmd.Flags().BoolVar(&reuseWindow, "reuse-window", false, "Open in the current Zed window instead of a new one")
	return cmd
}

type toolsZedOptions struct {
	sandboxArg  string
	source      string
	editor      string
	reuseWindow bool
	args        []string
}

func (a *App) runToolsZed(cmd *cobra.Command, opts toolsZedOptions) error {
	// Resolved before anything is written, for the reason `tools vscode`
	// resolves it there: a missing editor is the one failure nothing can fix
	// after the fact.
	editor, err := zedFamily.resolve(opts.editor)
	if err != nil {
		return err
	}
	notes := printedNotes(cmd.ErrOrStderr())
	targets, err := zedFamily.sshTargets(cmd.Context(), editor, notes)
	if err != nil {
		return err
	}

	remote, editorArgs, err := a.editorRemote(cmd, targets, opts.sandboxArg, opts.source, opts.args, notes)
	if err != nil {
		return err
	}

	var full []string
	if opts.reuseWindow {
		full = append(full, "--reuse")
	} else {
		// A new window by default, for the reason `tools vscode` opens one:
		// the window you typed this in is on something else.
		full = append(full, "--new")
	}
	full = append(full, remote.zedURL())
	full = append(full, editorArgs...)

	fmt.Fprintf(cmd.ErrOrStderr(), "opening %s in %s\n", remote.describe(), editor)
	return zedFamily.launch(cmd, editor, full)
}
