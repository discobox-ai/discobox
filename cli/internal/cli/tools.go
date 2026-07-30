package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// newToolsCommand groups the commands that run a familiar development tool
// inside a sandbox against one of its sources, with the tool's own arguments
// passed through untouched.
func (a *App) newToolsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tools",
		Aliases: []string{"tool", "t"},
		Short:   "Run a development tool inside a sandbox",
		Long: `Run a development tool inside a sandbox, in the working tree of one of its
sources.

Without --sandbox-id the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick. Without --source the tool runs in the sandbox's default working
directory, which is its primary source.`,
	}
	cmd.AddCommand(a.newToolsGitCommand())
	return cmd
}

// toolsOptions is the sandbox and source selection every tools subcommand
// shares. The flags live on each subcommand so they can be parsed before the
// tool's own arguments, which are passed through untouched.
type toolsOptions struct {
	sandboxID string
	source    string
}

func (o *toolsOptions) addFlags(a *App, cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.sandboxID, "sandbox-id", "", "Sandbox to run in; when omitted, the sandbox started from this directory, or a prompt to pick one")
	_ = cmd.RegisterFlagCompletionFunc("sandbox-id", a.completeSandboxes)
	cmd.Flags().StringVarP(&o.source, "source", "s", "", "Source to run in, named by its slug; defaults to the sandbox's primary source")
}

func (a *App) newToolsGitCommand() *cobra.Command {
	var opts toolsOptions
	cmd := &cobra.Command{
		Use:   "git [flags] [--] ARG [ARG...]",
		Short: "Run git in a sandbox source's working tree",
		Long: `Run git inside a sandbox, in the working tree of one of its sources.

Every argument is passed to git as is; only this command's own flags, given
before the git arguments, are consumed here. Use -- when a git argument would
otherwise be read as one of them.`,
		Example: `  disco tools git status
  disco tools git -s docs log --oneline -5
  disco tools git --sandbox-id sbx_01hq diff
  disco tools git -- --version`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runToolInSource(cmd, opts, append([]string{"git"}, args...))
		},
	}
	// Stop parsing flags at the first positional argument so everything from
	// there on belongs to git: `disco tools git log -s` sends -s to git, while
	// `disco tools git -s docs log` still selects the source here.
	cmd.Flags().SetInterspersed(false)
	opts.addFlags(a, cmd)
	return cmd
}

// runToolInSource runs command inside the sandbox, in the working tree of the
// selected source, streamed to this terminal exactly like `disco exec`.
func (a *App) runToolInSource(cmd *cobra.Command, opts toolsOptions, command []string) error {
	ctx := cmd.Context()
	projectID, sandboxID, client, err := a.selectSandbox(cmd, opts.sandboxID)
	if err != nil {
		return err
	}
	// The primary source needs no workdir and therefore no sandbox record: an
	// exec with no workdir already lands in the sandbox's default exec
	// directory, which is the primary source's. Naming another source with
	// --source is the only thing that has to look one up, so the common
	// `disco t git status` is create + attach and nothing else.
	workdir := ""
	if opts.source != "" {
		workdir, err = a.toolSourceWorkdir(ctx, client, projectID, sandboxID, opts.source)
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
			return "", fmt.Errorf("source %s has no known directory in the sandbox", slug)
		}
		return workdir, nil
	}
	return "", fmt.Errorf("sandbox has no source with slug %q", slug)
}
