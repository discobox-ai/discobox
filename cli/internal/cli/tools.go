package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// newToolsCommand groups the commands that run a familiar development tool
// against a sandbox, with the tool's own arguments passed through untouched.
// Where the tool itself runs is the tool's business: `git` runs inside the
// sandbox, while `ssh` and `vscode` run here and connect to it.
func (a *App) newToolsCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "tools",
		Aliases: []string{"tool", "t"},
		Short:   "Run a development tool against a discobox",
		Long: `Run a development tool against a discobox.

Without --discobox-id the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Each tool decides where it runs and what else it needs: git runs inside the
discobox and takes --source, ssh and vscode run here and connect to the
discobox.`,
	}
	// Which sandbox a tool acts on is the one thing every tool has in common,
	// so it is asked once here and inherited. Everything else, including where
	// the tool runs, belongs to the subcommand that means it.
	cmd.PersistentFlags().StringVar(&sandboxID, "discobox-id", "", "Discobox to act on; when omitted, the discobox started from this directory, or a prompt to pick one")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)

	cmd.AddCommand(a.newToolsGitCommand(&sandboxID))
	cmd.AddCommand(a.newToolsSSHCommand(&sandboxID))
	cmd.AddCommand(a.newToolsVSCodeCommand(&sandboxID))
	return cmd
}

func (a *App) newToolsGitCommand(sandboxID *string) *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "git [flags] [--] ARG [ARG...]",
		Short: "Run git in a discobox source's working tree",
		Long: `Run git inside a discobox, in the working tree of one of its sources.

Without --source git runs in the discobox's default working directory, which is
its primary source.

Every argument is passed to git as is; only the flags before the git arguments
are consumed here. Use -- when a git argument would otherwise be read as one of
them.`,
		Example: `  discobox tools git status
  discobox tools git -s docs log --oneline -5
  discobox tools git --discobox-id sbx_01hq diff
  discobox tools git -- --version`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runToolInSource(cmd, *sandboxID, source, append([]string{"git"}, args...))
		},
	}
	// Stop parsing flags at the first positional argument so everything from
	// there on belongs to git: `discobox tools git log -s` sends -s to git, while
	// `discobox tools git -s docs log` still selects the source here.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVarP(&source, "source", "s", "", "Source to run in, named by its slug; defaults to the discobox's primary source")
	return cmd
}

// runToolInSource runs command inside the sandbox, in the working tree of the
// selected source, streamed to this terminal exactly like `discobox shell`.
func (a *App) runToolInSource(cmd *cobra.Command, sandboxArg, sourceSlug string, command []string) error {
	ctx := cmd.Context()
	projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
	if err != nil {
		return err
	}
	// The primary source needs no workdir and therefore no sandbox record: an
	// exec with no workdir already lands in the sandbox's default exec
	// directory, which is the primary source's. Naming another source with
	// --source is the only thing that has to look one up, so the common
	// `discobox t git status` is create + attach and nothing else.
	workdir := ""
	if sourceSlug != "" {
		workdir, err = a.toolSourceWorkdir(ctx, client, projectID, sandboxID, sourceSlug)
		if err != nil {
			return err
		}
	}

	tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
	body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty, workdir: workdir}, command)
	if err != nil {
		return err
	}
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body)
	if err != nil {
		return err
	}
	if err := a.attachSandboxExec(ctx, projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	return a.returnSandboxExecStatus(ctx, projectID, sandboxID, exec.ID)
}

// toolSourceWorkdir is the directory the source named by slug lives at inside
// the sandbox, from the sandbox record.
func (a *App) toolSourceWorkdir(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, slug string) (string, error) {
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return "", err
	}
	for _, entry := range applySources(sandbox) {
		if entry.slug != slug {
			continue
		}
		workdir := sourceWorkdir(entry.source)
		if workdir == "" {
			return "", fmt.Errorf("source %s has no known directory in the discobox", slug)
		}
		return workdir, nil
	}
	return "", fmt.Errorf("discobox has no source with slug %q", slug)
}
