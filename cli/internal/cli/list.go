package cli

import (
	"context"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// newListCommand lists this machine's discoboxes for -C (the working directory
// by default): the ones cut from it here, and the ones created here with no
// source (ADR 0111). It is the project-scoped counterpart to `admin box ls`,
// which lists every sandbox in the project regardless of where it came from.
//
// It filters on the origin key, which is the host as well as where the source
// came from: a local path identifies a repository only on the machine holding
// it, so the machine has to be part of the answer once the server is remote.
func (a *App) newListCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"ps"},
		Short:   "List this machine's discoboxes for the current directory",
		Long: `List the discoboxes this machine cut from the current directory.

Those are the discoboxes whose source is the Git repository root of the current
directory (or the directory itself when it is not in a repository), or the
repository URL -C names, plus this machine's discoboxes with no source.
Discoboxes cut from anywhere else, or created on another machine, are not
listed; pass --all (or use "discobox admin box ls") to list every discobox in
the project.`,
		Example: `  discobox ls
  discobox ls --all
  discobox ls -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			sandboxes, err := a.listProjectSandboxes(cmd.Context(), client, projectID, all)
			if err != nil {
				return err
			}
			return a.writeSandboxes(cmd, sandboxes, all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "List every discobox in the project, whatever it was cut from and on whichever machine, and show a SOURCE column")
	a.addQuietFlag(cmd)
	return cmd
}

// listProjectSandboxes lists the project's sandboxes, filtered to this
// machine's for -C unless all is set: the two origin keys OriginKeys names. It is the
// listing behind `discobox ls`, shared with the commands that ask the user to pick
// one of those sandboxes.
func (a *App) listProjectSandboxes(ctx context.Context, client *apiclientgen.Client, projectID string, all bool) ([]apimodel.Sandbox, error) {
	params := apiclientgen.ListSandboxesParams{ProjectId: projectID}
	if !all {
		sourceKey, hostKey, err := sandboxcreate.OriginKeys(ctx, a.source)
		if err != nil {
			return nil, err
		}
		params.OriginKey = []string{sourceKey, hostKey}
	}
	bodyRes, err := client.ListSandboxes(ctx, params)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](bodyRes)
	if err != nil {
		return nil, err
	}
	return body.GetSandboxes(), nil
}
