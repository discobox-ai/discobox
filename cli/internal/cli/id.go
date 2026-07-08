package cli

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	idpkg "github.com/obot-platform/discobox/id"
)

func shortID(id string) string {
	return idpkg.Short(id)
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
	if !isResolvableShortID(id) {
		return id, nil
	}
	var matches []string
	for _, candidate := range ids {
		if strings.HasSuffix(candidate, id) || strings.HasPrefix(candidate, id) {
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

func isResolvableShortID(id string) bool {
	if !idpkg.IsShort(id) || idpkg.IsFriendly(id) {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') {
			continue
		}
		return false
	}
	return true
}

func (a *App) resolveSandboxID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "sandbox ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
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
	if err != nil {
		return id, err
	}
	res, err := client.ListAgentConfigs(ctx, apiclientgen.ListAgentConfigsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListAgentConfigsBody](res)
	if err != nil {
		return "", err
	}
	agents := body.GetAgentConfigs()
	// Prefer the stable slug (e.g. "codex"), then the display name, so the agent
	// subcommands accept the same selectors as `sandbox create --agent` and `run -a`.
	for _, agent := range agents {
		if agent.Slug == value {
			return agent.ID, nil
		}
	}
	for _, agent := range agents {
		if agent.Name == value {
			return agent.ID, nil
		}
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(agents))
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	return resolveShortID(id, "agent config ID", ids)
}

func (a *App) resolveAgentDefinitionID(ctx context.Context, client *apiclientgen.Client, value string) (string, error) {
	id, err := parseIDArg(value, "agent definition ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListAgentConfigDefinitions(ctx)
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListAgentConfigDefinitionsBody](res)
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
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListSandboxProviderInstances(ctx, apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSandboxProviderInstancesBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetProviders()))
	for _, provider := range body.GetProviders() {
		ids = append(ids, provider.ID)
	}
	return resolveShortID(id, "provider ID", ids)
}

func (a *App) resolveSecretID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "secret ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListSecrets(ctx, apiclientgen.ListSecretsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSecretsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetSecrets()))
	for _, secret := range body.GetSecrets() {
		ids = append(ids, secret.ID)
	}
	return resolveShortID(id, "secret ID", ids)
}

func (a *App) resolveSecretRequestID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "secret request ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListSecretRequests(ctx, apiclientgen.ListSecretRequestsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSecretRequestsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetSecretRequests()))
	for _, request := range body.GetSecretRequests() {
		ids = append(ids, request.ID)
	}
	return resolveShortID(id, "secret request ID", ids)
}

func (a *App) resolveJobID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "job ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListJobs(ctx, apiclientgen.ListJobsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListJobsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetJobs()))
	for _, job := range body.GetJobs() {
		ids = append(ids, job.ID)
	}
	return resolveShortID(id, "job ID", ids)
}
