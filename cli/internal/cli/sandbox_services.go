package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// Services are the discobox's own long-running processes: the scripts its
// repository declares under `.discobox/services`, started for you when the
// sandbox boots (ADR 0063). They are addressed by their declared id rather
// than by an exec id, because the declaration is what outlives any one run.

func (a *App) newSandboxServiceCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "services",
		Aliases: []string{"service"},
		Short:   "Manage a sandbox's declared services",
		Long: "Services are the scripts a repository declares under .discobox/services.\n" +
			"The sandbox starts them when it boots; these commands inspect and control them afterwards.",
	}
	cmd.PersistentFlags().StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	_ = cmd.RegisterFlagCompletionFunc("sandbox-id", a.completeSandboxes)
	cmd.AddCommand(a.newSandboxServiceListCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxServiceLifecycleCommand(&sandboxID, "start", "Start declared services"))
	cmd.AddCommand(a.newSandboxServiceLifecycleCommand(&sandboxID, "stop", "Stop running services"))
	cmd.AddCommand(a.newSandboxServiceLifecycleCommand(&sandboxID, "restart", "Restart declared services"))
	cmd.AddCommand(a.newSandboxServiceLogsCommand(&sandboxID))
	return cmd
}

func (a *App) newSandboxServiceListCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List declared services",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxServiceRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			services, err := listSandboxServices(cmd.Context(), client, projectID, resolvedSandboxID)
			if err != nil {
				return err
			}
			return a.writeSandboxServices(cmd, services)
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

// newSandboxServiceLifecycleCommand builds start, stop and restart. They differ
// only in which route they post to and what they are called, so the argument
// handling, the multi-service loop and the reporting are written once.
func (a *App) newSandboxServiceLifecycleCommand(sandboxID *string, verb, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:               verb + " SERVICE [SERVICE...]",
		Short:             short,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeServices(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxServiceRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			changed := make([]apimodel.SandboxService, 0, len(args))
			for _, id := range args {
				service, err := actOnSandboxService(cmd.Context(), client, verb, projectID, resolvedSandboxID, id)
				if err != nil {
					return fmt.Errorf("%s %s: %w", verb, id, err)
				}
				changed = append(changed, *service)
			}
			return a.writeSandboxServices(cmd, changed)
		},
	}
	return cmd
}

func (a *App) newSandboxServiceLogsCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "logs SERVICE",
		Short:             "Print a service's output",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeServices(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxServiceRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			res, err := client.ListSandboxServiceLogs(cmd.Context(), apiclientgen.ListSandboxServiceLogsParams{
				ProjectId: projectID,
				SandboxId: resolvedSandboxID,
				ServiceId: args[0],
			})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.SandboxExecLogsResponse](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), body)
			}
			// A service runs on pipes rather than a PTY (ADR 0063 §3), so its
			// two streams are still distinct here and are written to the two
			// this command was given. There is no input to include: nothing
			// types at a service.
			return writeSandboxExecLogs(cmd.OutOrStdout(), cmd.ErrOrStderr(), body.GetEntries(), false)
		},
	}
	return cmd
}

func (a *App) sandboxServiceRequest(ctx context.Context, sandboxArg string) (string, string, *apiclientgen.Client, error) {
	if strings.TrimSpace(sandboxArg) == "" {
		return "", "", nil, fmt.Errorf("--sandbox-id is required")
	}
	return a.sandboxRequest(ctx, sandboxArg)
}

func listSandboxServices(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) ([]apimodel.SandboxService, error) {
	res, err := client.ListSandboxServices(ctx, apiclientgen.ListSandboxServicesParams{
		ProjectId: projectID,
		SandboxId: sandboxID,
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.SandboxServicesResponse](res)
	if err != nil {
		return nil, err
	}
	return body.GetServices(), nil
}

func actOnSandboxService(ctx context.Context, client *apiclientgen.Client, verb, projectID, sandboxID, serviceID string) (*apimodel.SandboxService, error) {
	switch verb {
	case "start":
		res, err := client.StartSandboxService(ctx, apiclientgen.StartSandboxServiceParams{
			ProjectId: projectID, SandboxId: sandboxID, ServiceId: serviceID,
		})
		if err != nil {
			return nil, err
		}
		return expectResponse[apimodel.SandboxService](res)
	case "stop":
		res, err := client.StopSandboxService(ctx, apiclientgen.StopSandboxServiceParams{
			ProjectId: projectID, SandboxId: sandboxID, ServiceId: serviceID,
		})
		if err != nil {
			return nil, err
		}
		return expectResponse[apimodel.SandboxService](res)
	case "restart":
		res, err := client.RestartSandboxService(ctx, apiclientgen.RestartSandboxServiceParams{
			ProjectId: projectID, SandboxId: sandboxID, ServiceId: serviceID,
		})
		if err != nil {
			return nil, err
		}
		return expectResponse[apimodel.SandboxService](res)
	default:
		return nil, fmt.Errorf("unknown service verb %q", verb)
	}
}

func (a *App) writeSandboxServices(cmd *cobra.Command, services []apimodel.SandboxService) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), services, func(service apimodel.SandboxService) string { return service.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"services": services})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPID\tEXIT\tNAME\tDESCRIPTION")
	for _, service := range services {
		pid := ""
		if value, ok := service.Pid.Get(); ok {
			pid = fmt.Sprint(value)
		}
		exitCode := ""
		if value, ok := service.ExitCode.Get(); ok {
			exitCode = fmt.Sprint(value)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			service.ID,
			service.Status,
			pid,
			exitCode,
			service.Name,
			truncateTableValue(sandboxServiceDetail(service), 60),
		)
	}
	return tw.Flush()
}

// sandboxServiceDetail is the last column: normally what the service is for,
// but a declaration that cannot run says so instead. The reason it cannot run
// is the more useful of the two, and a service showing `stopped` with no
// explanation is exactly the confusion this avoids.
func sandboxServiceDetail(service apimodel.SandboxService) string {
	if problem, ok := service.Problem.Get(); ok && strings.TrimSpace(problem) != "" {
		return "cannot run: " + problem
	}
	if detail, ok := service.Error.Get(); ok && strings.TrimSpace(detail) != "" {
		return detail
	}
	return service.Description.Or("")
}

func (a *App) listServiceCompletions(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) ([]string, error) {
	services, err := listSandboxServices(ctx, client, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	completions := make([]string, 0, len(services))
	for _, service := range services {
		completion := service.ID
		if description := strings.TrimSpace(service.Description.Or("")); description != "" {
			completion += "\t" + description
		}
		completions = append(completions, completion)
	}
	sort.Strings(completions)
	return completions, nil
}
