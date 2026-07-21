package cli

import (
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
)

// newListCommand lists the sandboxes started from the current project
// directory, which is the Git repository root of -C (the working directory by
// default). It is the project-scoped counterpart to `box sandbox ls`, which
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
		Short:   "List the sandboxes started from the current directory",
		Long: `List the sandboxes started from the current project directory.

The project directory is the Git repository root of the current directory, or
the directory itself when it is not in a repository. Sandboxes started from
another directory, or from another machine, are not listed; pass --all (or use
"disco box sandbox ls") to list every sandbox in the project.`,
		Example: `  disco ls
  disco ls --all
  disco ls -o json`,
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
			params := apiclientgen.ListSandboxesParams{ProjectId: projectID}
			if !all {
				originKey, err := sandboxcreate.OriginKey(cmd.Context(), a.source)
				if err != nil {
					return err
				}
				params.OriginKey = apiclientgen.NewOptString(originKey)
			}
			bodyRes, err := client.ListSandboxes(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSandboxesBody](bodyRes)
			if err != nil {
				return err
			}
			return a.writeSandboxes(cmd, body.GetSandboxes(), all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "List every sandbox in the project, regardless of the directory it was started from, and show a FOLDER column")
	a.addQuietFlag(cmd)
	return cmd
}
