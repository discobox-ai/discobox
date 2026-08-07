package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// copyableResources are the things `project create --copy` can take from the
// source project, in the order the server applies them.
var copyableResources = []string{"providers", "pools", "harnesses"}

type projectCreateOptions struct {
	from string
	copy []string
}

func (a *App) newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Aliases: []string{"projects"}, Short: "Manage projects"}
	cmd.AddCommand(a.newProjectListCommand())
	cmd.AddCommand(a.newProjectGetCommand())
	cmd.AddCommand(a.newProjectCreateCommand())
	cmd.AddCommand(a.newProjectUpdateCommand())
	cmd.AddCommand(a.newProjectSetDefaultCommand())
	cmd.AddCommand(a.newProjectDeleteCommand())
	return cmd
}

func (a *App) newProjectListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List projects", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		res, err := client.ListProjects(cmd.Context())
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListProjectsBody](res)
		if err != nil {
			return err
		}
		return a.writeProjects(cmd, body.GetProjects())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newProjectGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get PROJECT_ID", Short: "Get a project", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeProjects, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.resolveProjectID(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		project, err := a.getProject(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		return a.writeProject(cmd, project)
	}}
}

func (a *App) newProjectCreateCommand() *cobra.Command {
	var opts projectCreateOptions
	cmd := &cobra.Command{Use: "create NAME", Short: "Create a project", Long: `Create a project.

A project is the ownership boundary every sandbox, pool, provider instance,
harness config, and secret lives inside. Every new project is seeded with the
built-in harnesses, unconfigured.

--from seeds the rest of it from an existing project: its provider instances
are duplicated, its pools are recreated against those copies as new hosts, and
its configured harnesses come across with the files, secrets, and grants their
configure flow produced. Pools imply providers, since a pool binds to a
provider instance in its own project.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		body := &apimodel.CreateProjectBody{Name: args[0]}
		if opts.from != "" {
			from, err := a.resolveProjectID(cmd.Context(), client, opts.from)
			if err != nil {
				return err
			}
			body.SetCopyFromProjectId(apiclientgen.NewOptString(from))
			copies, err := parseCopyItems(opts.copy)
			if err != nil {
				return err
			}
			body.SetCopy(apiclientgen.NewOptNilCreateProjectBodyCopyItemArray(copies))
		} else if cmd.Flags().Changed("copy") {
			return fmt.Errorf("--copy needs --from: there is nothing to copy from")
		}
		res, err := client.CreateProject(cmd.Context(), body)
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		return a.writeProject(cmd, project)
	}}
	cmd.Flags().StringVar(&opts.from, "from", "", "Project to copy configuration from")
	_ = cmd.RegisterFlagCompletionFunc("from", a.completeProjects)
	cmd.Flags().StringSliceVar(&opts.copy, "copy", copyableResources,
		"What to copy from --from: "+strings.Join(copyableResources, ", ")+", or none")
	return cmd
}

func (a *App) newProjectUpdateCommand() *cobra.Command {
	var name string
	var archiveRetention time.Duration
	cmd := &cobra.Command{Use: "update PROJECT_ID", Short: "Update a project", Long: `Update a project.

--archive-retention sets how long this project's archived sandboxes are kept
before they are purged. Deleting a sandbox archives it, so this is how long a
deleted sandbox can still be restored with "disco box sandbox unarchive".
Zero restores the server default.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeProjects, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.resolveProjectID(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		body := &apimodel.UpdateProjectBody{}
		if cmd.Flags().Changed("name") {
			body.SetName(apiclientgen.NewOptString(name))
		}
		if cmd.Flags().Changed("archive-retention") {
			if archiveRetention < 0 {
				return fmt.Errorf("--archive-retention cannot be negative")
			}
			body.SetArchiveRetentionSeconds(apiclientgen.NewOptInt64(int64(archiveRetention / time.Second)))
		}
		res, err := client.UpdateProject(cmd.Context(), body, apiclientgen.UpdateProjectParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		return a.writeProject(cmd, project)
	}}
	cmd.Flags().StringVar(&name, "name", "", "Project display name")
	cmd.Flags().DurationVar(&archiveRetention, "archive-retention", 0, "How long archived sandboxes are kept before being purged (e.g. 48h); 0 restores the server default")
	return cmd
}

func (a *App) newProjectSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default PROJECT_ID", Short: "Set your default project", Long: `Set your default project.

Commands run against the default project unless --project names another one,
so this is what "default" resolves to everywhere. There is no unset: setting a
default moves the flag off whichever project holds it.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeProjects, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.resolveProjectID(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		res, err := client.SetDefaultProject(cmd.Context(), apiclientgen.SetDefaultProjectParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), project)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "default project set to %s\n", project.ID)
		return err
	}}
}

func (a *App) newProjectDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete PROJECT_ID...", Short: "Delete projects", Long: `Delete projects.

A project with sandboxes or pools is refused: those own runtime hosts and
containers that have to be torn down through their own commands first. The
default project is refused too; make another project the default first.`, Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completeProjects, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "project", "deleted", func(arg string) (string, error) {
			projectID, err := a.resolveProjectID(cmd.Context(), client, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteProject(cmd.Context(), apiclientgen.DeleteProjectParams{ProjectId: projectID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteProjectNoContent](res); err != nil {
				return "", err
			}
			return projectID, nil
		})
	}}
}

func (a *App) getProject(ctx context.Context, client *apiclientgen.Client, projectID string) (*apimodel.Project, error) {
	res, err := client.GetProject(ctx, apiclientgen.GetProjectParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	return expectResponse[apimodel.Project](res)
}

// parseCopyItems turns --copy values into the API's copy items. "none" is the
// spelling for an empty selection, since a flag with a non-empty default needs
// a way to say "nothing" that an empty string does not read as.
func parseCopyItems(values []string) ([]apiclientgen.CreateProjectBodyCopyItem, error) {
	items := make([]apiclientgen.CreateProjectBodyCopyItem, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "", "none":
			continue
		case "providers", "pools", "harnesses":
			items = append(items, apiclientgen.CreateProjectBodyCopyItem(value))
		default:
			return nil, fmt.Errorf("unknown --copy value %q; expected %s, or none", value, strings.Join(copyableResources, ", "))
		}
	}
	return items, nil
}
