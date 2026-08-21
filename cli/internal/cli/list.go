package cli

import (
	"context"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// newListCommand lists the sandboxes started from the current project
// directory, which is the Git repository root of -C (the working directory by
// default). It is the project-scoped counterpart to `admin discobox ls`, which
// lists every sandbox in the project regardless of where it was started from.
//
// This filters on origin rather than source: a local repository path identifies
// a repository only on the machine holding it, so it cannot answer "sandboxes I
// started here" once the server is remote.
func (a *App) newListCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"ps"},
		Short:   "List the discoboxes started from the current directory",
		Long: `List the discoboxes started from the current project directory.

The project directory is the Git repository root of the current directory, or
the directory itself when it is not in a repository. Discoboxes started from
another directory, or from another machine, are not listed; pass --all (or use
"discobox admin discobox ls") to list every discobox in the project.`,
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
	cmd.Flags().BoolVarP(&all, "all", "a", false, "List every discobox in the project, regardless of the directory it was started from, and show a FOLDER column")
	a.addQuietFlag(cmd)
	return cmd
}

// listProjectSandboxes lists the project's sandboxes, filtered to the ones
// started from the current project directory unless all is set. It is the
// listing behind `discobox ls`, shared with the commands that ask the user to pick
// one of those sandboxes.
func (a *App) listProjectSandboxes(ctx context.Context, client *apiclientgen.Client, projectID string, all bool) ([]apimodel.Sandbox, error) {
	params := apiclientgen.ListSandboxesParams{ProjectId: projectID}
	if !all {
		originKey, err := sandboxcreate.OriginKey(ctx, a.source)
		if err != nil {
			return nil, err
		}
		params.OriginKey = apiclientgen.NewOptString(originKey)
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
