package cli

import (
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/clientgen"
)

func (a *App) newJobCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "job",
		Aliases: []string{"jobs"},
		Short:   "Inspect durable jobs",
		RunE:    a.runJobList,
	}
	cmd.AddCommand(a.newJobListCommand())
	cmd.AddCommand(a.newJobGetCommand())
	cmd.AddCommand(a.newJobRunNowCommand())
	return cmd
}

func (a *App) newJobListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List jobs for the current project",
		RunE:  a.runJobList,
	}
}

func (a *App) newJobGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get JOB_ID",
		Short: "Get a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			jobID, err := a.resolveJobID(cmd.Context(), client, projectID, args[0])
			if err != nil {
				return err
			}
			res, err := client.GetJob(cmd.Context(), apiclientgen.GetJobParams{ProjectId: projectID, JobId: jobID})
			if err != nil {
				return err
			}
			job, err := expectResponse[apiclientgen.Job](res)
			if err != nil {
				return err
			}
			return a.writeJob(cmd, job)
		},
	}
}

func (a *App) newJobRunNowCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "run-now JOB_ID",
		Aliases: []string{"force"},
		Short:   "Run a pending or backoff job now",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			jobID, err := a.resolveJobID(cmd.Context(), client, projectID, args[0])
			if err != nil {
				return err
			}
			res, err := client.ForceJob(cmd.Context(), apiclientgen.ForceJobParams{ProjectId: projectID, JobId: jobID})
			if err != nil {
				return err
			}
			job, err := expectResponse[apiclientgen.Job](res)
			if err != nil {
				return err
			}
			return a.writeJob(cmd, job)
		},
	}
}

func (a *App) runJobList(cmd *cobra.Command, _ []string) error {
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	res, err := client.ListJobs(cmd.Context(), apiclientgen.ListJobsParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	body, err := expectResponse[apiclientgen.ListJobsBody](res)
	if err != nil {
		return err
	}
	return a.writeJobs(cmd, body.GetJobs())
}
