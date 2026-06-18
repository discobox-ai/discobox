package cli

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/clientgen"
)

const shortIDLength = 8

func shortID(id string) string {
	if len(id) <= shortIDLength {
		return id
	}
	return id[len(id)-shortIDLength:]
}

func shortResourceID(resourceType, resourceID string) string {
	if resourceType == "" {
		return shortID(resourceID)
	}
	return resourceType + "/" + shortID(resourceID)
}

func resolveShortID(value, name string, ids []string) (string, error) {
	id, err := parseIDArg(value, name)
	if err != nil {
		return "", err
	}
	if len(id) != shortIDLength {
		return id, nil
	}
	var matches []string
	for _, candidate := range ids {
		if strings.HasSuffix(candidate, id) {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s matches short ID %q", name, id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("short %s %q is ambiguous; matches %s", name, id, strings.Join(matches, ", "))
	}
}

func (a *App) resolveSandboxID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "sandbox ID")
	if err != nil || len(id) != shortIDLength {
		return id, err
	}
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apiclientgen.ListSandboxesBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetSandboxes()))
	for _, sandbox := range body.GetSandboxes() {
		ids = append(ids, sandbox.ID)
	}
	return resolveShortID(id, "sandbox ID", ids)
}

func (a *App) resolveAgentConfigID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "agent config ID")
	if err != nil || len(id) != shortIDLength {
		return id, err
	}
	res, err := client.ListAgentConfigs(ctx, apiclientgen.ListAgentConfigsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apiclientgen.ListAgentConfigsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetAgentConfigs()))
	for _, agent := range body.GetAgentConfigs() {
		ids = append(ids, agent.ID)
	}
	return resolveShortID(id, "agent config ID", ids)
}

func (a *App) resolveAgentDefinitionID(ctx context.Context, client *apiclientgen.Client, value string) (string, error) {
	id, err := parseIDArg(value, "agent definition ID")
	if err != nil || len(id) != shortIDLength {
		return id, err
	}
	res, err := client.ListAgentConfigDefinitions(ctx)
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apiclientgen.ListAgentConfigDefinitionsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetAgentConfigDefinitions()))
	for _, definition := range body.GetAgentConfigDefinitions() {
		ids = append(ids, definition.ID)
	}
	return resolveShortID(id, "agent definition ID", ids)
}

func (a *App) resolveProviderID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "provider ID")
	if err != nil || len(id) != shortIDLength {
		return id, err
	}
	res, err := client.ListSandboxProviderInstances(ctx, apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apiclientgen.ListSandboxProviderInstancesBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetProviders()))
	for _, provider := range body.GetProviders() {
		ids = append(ids, provider.ID)
	}
	return resolveShortID(id, "provider ID", ids)
}

func (a *App) resolveJobID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "job ID")
	if err != nil || len(id) != shortIDLength {
		return id, err
	}
	res, err := client.ListJobs(ctx, apiclientgen.ListJobsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apiclientgen.ListJobsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetJobs()))
	for _, job := range body.GetJobs() {
		ids = append(ids, job.ID)
	}
	return resolveShortID(id, "job ID", ids)
}
