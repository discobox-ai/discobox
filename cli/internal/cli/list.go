package cli

import (
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
)

// newListCommand lists the sandboxes of the current source repository, which is
// the Git repository root of -C (the working directory by default). It is the
// repository-scoped counterpart to `box sandbox ls`, which lists every sandbox in
// the project regardless of what it runs against.
func (a *App) newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"ps"},
		Short:   "List the sandboxes running against the current repository",
		Long: `List the sandboxes running against the current repository.

The repository is the Git repository root of the current directory. Sandboxes
created from another repository, or from no repository at all, are not listed;
use "disco box sandbox ls" to list every sandbox in the project.`,
		Example: `  disco ls
  disco ls -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			sourceRoot, err := sandboxcreate.ResolveSourceRoot(cmd.Context(), a.source)
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			bodyRes, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{
				ProjectId:  projectID,
				SourceRoot: apiclientgen.NewOptString(sourceRoot),
			})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSandboxesBody](bodyRes)
			if err != nil {
				return err
			}
			return a.writeSandboxes(cmd, body.GetSandboxes())
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}
