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

func (a *App) completeProjects(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	client, err := a.apiClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	res, err := client.ListProjects(commandContext(cmd))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	body, err := expectResponse[apimodel.ListProjectsBody](res)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	completions := []string{completionItem(defaultProjectAlias, "default project")}
	for _, project := range body.GetProjects() {
		if project.ID != "" {
			completions = append(completions, completionItem(project.ID, project.Name))
		}
	}
	return filterCompletions(completions, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (a *App) completeSandboxes(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listSandboxCompletions)
}

func (a *App) completeProviders(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listProviderCompletions)
}

func (a *App) completePools(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listPoolCompletions)
}

func (a *App) completeHarnessConfigs(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listHarnessConfigCompletions)
}

func (a *App) completeHarnessConfigNames(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listHarnessConfigNameCompletions)
}

func (a *App) completeJobs(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.completeProjectResource(cmd, toComplete, a.listJobCompletions)
}

func (a *App) completeTerminals(sandboxID *string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return a.completeSandboxScopedResource(cmd, sandboxID, toComplete, a.listTerminalCompletions)
	}
}

func (a *App) completeExecs(sandboxID *string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return a.completeSandboxScopedResource(cmd, sandboxID, toComplete, a.listExecCompletions)
	}
}

func (a *App) completeProjectResource(cmd *cobra.Command, toComplete string, list func(context.Context, *apiclientgen.Client, string) ([]string, error)) ([]string, cobra.ShellCompDirective) {
	projectID, err := a.projectIDValue()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := a.apiClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	completions, err := list(commandContext(cmd), client, projectID)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterCompletions(completions, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (a *App) completeSandboxScopedResource(cmd *cobra.Command, sandboxID *string, toComplete string, list func(context.Context, *apiclientgen.Client, string, string) ([]string, error)) ([]string, cobra.ShellCompDirective) {
	if sandboxID == nil || strings.TrimSpace(*sandboxID) == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := a.apiClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx := commandContext(cmd)
	resolvedSandboxID, err := a.resolveSandboxID(ctx, client, projectID, *sandboxID)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	completions, err := list(ctx, client, projectID, resolvedSandboxID)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return filterCompletions(completions, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (a *App) listSandboxCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return nil, err
	}
	sandboxes := sortedByRecency(body.GetSandboxes(), func(sandbox apimodel.Sandbox) time.Time { return recencyTime(sandbox.UpdatedAt, sandbox.CreatedAt) })
	completions := make([]string, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		completions = append(completions, completionItem(sandbox.ID, completionDescription(sandbox.Config.Name, sandboxDisplayState(sandbox))))
	}
	return completions, nil
}

func (a *App) listProviderCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListSandboxProviderInstances(ctx, apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxProviderInstancesBody](res)
	if err != nil {
		return nil, err
	}
	providers := sortedByRecency(body.GetProviders(), func(provider apimodel.SandboxProviderInstance) time.Time {
		return recencyTime(provider.UpdatedAt, provider.CreatedAt)
	})
	completions := make([]string, 0, len(providers))
	for _, provider := range providers {
		completions = append(completions, completionItem(provider.ID, completionDescription(provider.Name, provider.Type)))
	}
	return completions, nil
}

func (a *App) listPoolCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return nil, err
	}
	pools := sortedByRecency(body.GetPools(), func(pool apimodel.Pool) time.Time { return recencyTime(pool.UpdatedAt, pool.CreatedAt) })
	completions := make([]string, 0, len(pools))
	for _, pool := range pools {
		completions = append(completions, completionItem(pool.ID, completionDescription(pool.Name, pool.ProviderInstanceId)))
	}
	return completions, nil
}

func (a *App) listHarnessConfigCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListHarnessConfigs(ctx, apiclientgen.ListHarnessConfigsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessConfigsBody](res)
	if err != nil {
		return nil, err
	}
	harnesses := sortedByRecency(body.GetHarnessConfigs(), func(harness apimodel.HarnessConfig) time.Time {
		return recencyTime(harness.UpdatedAt, harness.CreatedAt)
	})
	completions := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		completions = append(completions, completionItem(harness.ID, completionDescription(harness.Name, strings.Join(harness.RunCommand, " "))))
	}
	return completions, nil
}

func (a *App) listHarnessConfigNameCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListHarnessConfigs(ctx, apiclientgen.ListHarnessConfigsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessConfigsBody](res)
	if err != nil {
		return nil, err
	}
	harnesses := sortedByRecency(body.GetHarnessConfigs(), func(harness apimodel.HarnessConfig) time.Time {
		return recencyTime(harness.UpdatedAt, harness.CreatedAt)
	})
	completions := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		completions = append(completions, completionItem(harness.Name, completionDescription(harness.ID, strings.Join(harness.RunCommand, " "))))
	}
	return completions, nil
}

func (a *App) listJobCompletions(ctx context.Context, client *apiclientgen.Client, projectID string) ([]string, error) {
	res, err := client.ListJobs(ctx, apiclientgen.ListJobsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListJobsBody](res)
	if err != nil {
		return nil, err
	}
	jobs := sortedByRecency(body.GetJobs(), func(job apimodel.Job) time.Time { return recencyTime(job.UpdatedAt, job.CreatedAt) })
	completions := make([]string, 0, len(jobs))
	for _, job := range jobs {
		completions = append(completions, completionItem(job.ID, completionDescription(job.Type, string(job.Status))))
	}
	return completions, nil
}

func (a *App) listTerminalCompletions(ctx context.Context, _ *apiclientgen.Client, projectID, sandboxID string) ([]string, error) {
	terminals, err := a.listSandboxTerminals(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	// The virtual primary id is offered alongside the real terminals: it is the
	// only selector that relaunches a stopped primary terminal.
	terminals = sortedByRecency(terminals, func(terminal apimodel.SandboxExec) time.Time { return terminal.CreatedAt })
	completions := make([]string, 0, len(terminals)+1)
	completions = append(completions, completionItem(primaryExecID, "the sandbox's primary terminal, relaunched if stopped"))
	for _, terminal := range terminals {
		completions = append(completions, completionItem(terminal.ID, completionDescription(terminal.HarnessId.Or(""), string(terminal.Status))))
	}
	return completions, nil
}

func (a *App) listExecCompletions(ctx context.Context, _ *apiclientgen.Client, projectID, sandboxID string) ([]string, error) {
	execs, err := a.listSandboxExecs(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	execs = sortedByRecency(execs, func(exec apimodel.SandboxExec) time.Time { return exec.CreatedAt })
	completions := make([]string, 0, len(execs))
	for _, exec := range execs {
		completions = append(completions, completionItem(exec.ID, completionDescription(strings.Join(exec.Command, " "), string(exec.Status))))
	}
	return completions, nil
}

func completionItem(value, description string) string {
	value = strings.TrimSpace(value)
	description = strings.TrimSpace(description)
	if value == "" {
		return ""
	}
	if description == "" {
		return value
	}
	return fmt.Sprintf("%s\t%s", value, strings.ReplaceAll(description, "\t", " "))
}

func completionDescription(parts ...string) string {
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, " ")
}

func filterCompletions(completions []string, toComplete string) []string {
	toComplete = strings.TrimSpace(toComplete)
	out := make([]string, 0, len(completions))
	seen := map[string]struct{}{}
	for _, completion := range completions {
		if completion == "" {
			continue
		}
		value, _, _ := strings.Cut(completion, "\t")
		if toComplete != "" && !strings.HasPrefix(value, toComplete) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, completion)
	}
	return out
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
