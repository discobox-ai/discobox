package cli

import (
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type workerListOptions struct {
	provider string
}

func (a *App) newWorkerCommand() *cobra.Command {
	var opts workerListOptions
	cmd := &cobra.Command{
		Use:     "worker",
		Aliases: []string{"workers"},
		Short:   "Manage provider workers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runWorkerListWithOptions(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.provider, "provider", "", "Filter by provider instance ID")
	cmd.AddCommand(a.newWorkerListCommand())
	return cmd
}

func (a *App) newWorkerListCommand() *cobra.Command {
	var opts workerListOptions
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workers for the current project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runWorkerListWithOptions(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.provider, "provider", "", "Filter by provider instance ID")
	return cmd
}

func (a *App) runWorkerListWithOptions(cmd *cobra.Command, opts workerListOptions) error {
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	params := apiclientgen.ListWorkersParams{ProjectId: projectID}
	if provider := strings.TrimSpace(opts.provider); provider != "" {
		providerID, err := a.resolveProviderID(cmd.Context(), client, projectID, provider)
		if err != nil {
			return err
		}
		params.Provider.SetTo(providerID)
	}
	res, err := client.ListWorkers(cmd.Context(), params)
	if err != nil {
		return err
	}
	body, err := expectResponse[apimodel.ListWorkersBody](res)
	if err != nil {
		return err
	}
	return a.writeWorkers(cmd, body.GetWorkers())
}
