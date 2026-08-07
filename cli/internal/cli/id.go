package cli

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	idpkg "github.com/obot-platform/discobox/id"
)

func shortResourceID(resourceType, resourceID string) string {
	if resourceType == "" {
		return resourceID
	}
	return resourceType + "/" + resourceID
}

func resolveShortID(value, name string, ids []string) (string, error) {
	id, err := parseIDArg(value, name)
	if err != nil {
		return "", err
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	matches := idpkg.ResolveShort(id, ids)
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
	if id == "" || idpkg.IsGenerated(id) {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// resolveProjectID accepts the same selectors the -p flag does — the "default"
// alias, a full or short ID, or the display name — and resolves them to a
// project ID. The alias is left for the server, which is the only side that
// knows which project carries the user's default flag.
func (a *App) resolveProjectID(ctx context.Context, client *apiclientgen.Client, value string) (string, error) {
	id, err := parseIDArg(value, "project ID")
	if err != nil {
		return "", err
	}
	if id == defaultProjectAlias || idpkg.IsGenerated(id) {
		return id, nil
	}
	res, err := client.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListProjectsBody](res)
	if err != nil {
		return "", err
	}
	projects := body.GetProjects()
	for _, project := range projects {
		if project.Name == value {
			return project.ID, nil
		}
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return resolveShortID(id, "project ID", ids)
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

func (a *App) resolveHarnessConfigID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "harness config ID")
	if err != nil {
		return id, err
	}
	res, err := client.ListHarnessConfigs(ctx, apiclientgen.ListHarnessConfigsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListHarnessConfigsBody](res)
	if err != nil {
		return "", err
	}
	harnesses := body.GetHarnessConfigs()
	// Prefer the stable slug (e.g. "codex"), then the display name, so the harness
	// subcommands accept the same selectors as `box sandbox create --harness` and `prompt -a`.
	for _, harness := range harnesses {
		if harness.Slug == value {
			return harness.ID, nil
		}
	}
	for _, harness := range harnesses {
		if harness.Name == value {
			return harness.ID, nil
		}
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		ids = append(ids, harness.ID)
	}
	return resolveShortID(id, "harness config ID", ids)
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

func (a *App) resolvePoolID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "pool ID")
	if err != nil {
		return "", err
	}
	// A full generated pool ID needs no lookup; anything else may be a display
	// name or short ID and resolves against the pool listing.
	if idpkg.IsGenerated(id) {
		return id, nil
	}
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return "", err
	}
	pools := body.GetPools()
	// Accept the pool's display name as a selector alongside its ID.
	for _, pool := range pools {
		if pool.Name == value {
			return pool.ID, nil
		}
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(pools))
	for _, pool := range pools {
		ids = append(ids, pool.ID)
	}
	return resolveShortID(id, "pool ID", ids)
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
