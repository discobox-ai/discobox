package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

const defaultStatusLimit = 5

type statusSnapshot struct {
	Sandboxes []apimodel.Sandbox                 `json:"sandboxes"`
	Workers   []apimodel.Worker                  `json:"workers"`
	Providers []apimodel.SandboxProviderInstance `json:"providers"`
	Jobs      []apimodel.Job                     `json:"jobs"`
}

func (a *App) newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show recent sandboxes, workers, providers, and jobs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			status, err := a.loadStatusSnapshot(cmd, client, projectID)
			if err != nil {
				return err
			}
			return a.writeStatus(cmd, status)
		},
	}
}

func (a *App) loadStatusSnapshot(cmd *cobra.Command, client *apiclientgen.Client, projectID string) (statusSnapshot, error) {
	sandboxesRes, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list sandboxes: %w", err)
	}
	sandboxesBody, err := expectResponse[apimodel.ListSandboxesBody](sandboxesRes)
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list sandboxes: %w", err)
	}

	workersRes, err := client.ListWorkers(cmd.Context(), apiclientgen.ListWorkersParams{ProjectId: projectID})
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list workers: %w", err)
	}
	workersBody, err := expectResponse[apimodel.ListWorkersBody](workersRes)
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list workers: %w", err)
	}

	providersRes, err := client.ListSandboxProviderInstances(cmd.Context(), apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list providers: %w", err)
	}
	providersBody, err := expectResponse[apimodel.ListSandboxProviderInstancesBody](providersRes)
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list providers: %w", err)
	}

	jobsRes, err := client.ListJobs(cmd.Context(), apiclientgen.ListJobsParams{ProjectId: projectID})
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list jobs: %w", err)
	}
	jobsBody, err := expectResponse[apimodel.ListJobsBody](jobsRes)
	if err != nil {
		return statusSnapshot{}, fmt.Errorf("list jobs: %w", err)
	}

	return statusSnapshot{
		Sandboxes: newestByCreatedAt(sandboxesBody.GetSandboxes(), func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt }, defaultStatusLimit),
		Workers:   newestByCreatedAt(workersBody.GetWorkers(), func(worker apimodel.Worker) time.Time { return worker.CreatedAt }, defaultStatusLimit),
		Providers: newestByCreatedAt(providersBody.GetProviders(), func(provider apimodel.SandboxProviderInstance) time.Time { return provider.CreatedAt }, defaultStatusLimit),
		Jobs:      newestByCreatedAt(jobsBody.GetJobs(), func(job apimodel.Job) time.Time { return job.CreatedAt }, defaultStatusLimit),
	}, nil
}
