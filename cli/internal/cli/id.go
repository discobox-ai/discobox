package cli

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	idpkg "github.com/discobox-ai/x/id"
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
	id, err := parseIDArg(value, "discobox ID")
	// The hyphenated spelling a discobox's hostname uses ("sbx-…") is the same
	// ID, copied from a shell prompt.
	id = idpkg.Canonical(idpkg.PrefixSandbox, id)
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	// Judges included: they are left out of a listing because they are not
	// what somebody asking what is in their project means, but a discobox you
	// can see the ID of is one you can name — to open a shell in it, to take
	// it away, to ask what state it is in (ADR 26-09-22-838 §1).
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{
		ProjectId:    projectID,
		IncludeJudge: apiclientgen.NewOptBool(true),
	})
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
	return resolveShortID(id, "discobox ID", ids)
}

// resolveSandboxReference turns a reference somebody typed into a sandbox ID,
// where the reference is one they read off `discobox ls` rather than a raw
// `admin box` ID: a name, a full ID or a short ID from the candidate
// listing in sandboxes (matchSandboxArg), and failing that a short ID resolved
// against the whole project. match is which names the caller's argument may be
// written as; a name that more than one candidate answers to is refused rather
// than picked between.
//
// sandboxes is every candidate the caller listed, from whichever directory and
// machine it was created, so a name is matched across the project. A
// configured name is unique there; a listed name is a window title, which two
// discoboxes can share, and one that more than one candidate answers to is
// refused with the IDs to choose between. A reference that matches no candidate is still tried as an ID:
// a caller that filtered sandboxes first — cp passes the runtime candidates —
// can be handed the ID of a discobox its list left out.
func (a *App) resolveSandboxReference(ctx context.Context, client *apiclientgen.Client, projectID, reference string, sandboxes []apimodel.Sandbox, match nameMatch) (string, error) {
	sandboxID, ok, err := matchSandboxArg(reference, sandboxes, match)
	if err != nil {
		return "", err
	}
	if ok {
		return sandboxID, nil
	}
	if !isResolvableShortID(idpkg.Canonical(idpkg.PrefixSandbox, reference)) {
		return "", unmatchedSandboxName(reference)
	}
	sandboxID, err = a.resolveSandboxID(ctx, client, projectID, reference)
	if err != nil {
		return "", unmatchedSandboxReference(reference, err)
	}
	return sandboxID, nil
}

// resolveListedSandbox resolves one reference by the root commands' rule: the
// NAME `discobox ls` prints, an ID, or a short ID, over the whole listing,
// archived discoboxes included. A full ID is taken as it stands without listing
// anything, so a discobox acting on one it created makes only the call its use
// names (ADR 26-10-08-447).
func (a *App) resolveListedSandbox(ctx context.Context, client *apiclientgen.Client, projectID, reference string) (string, error) {
	var sandboxes []apimodel.Sandbox
	if !allFullSandboxIDs([]string{reference}) {
		var err error
		sandboxes, err = a.listProjectSandboxes(ctx, client, projectID, sandboxFilter{judges: true})
		if err != nil {
			return "", err
		}
	}
	return a.resolveSandboxReference(ctx, client, projectID, reference, sandboxes, listedName)
}

// unmatchedSandboxName is a reference that is no ID and names none of the
// candidates, which are every discobox the caller listed: there is nowhere
// wider to look.
func unmatchedSandboxName(reference string) error {
	return fmt.Errorf("no discobox named %q; run `discobox ls` to see them, or write the discobox ID", reference)
}

// unmatchedSandboxReference is a reference shaped like a short ID that named
// none of the candidates and failed project-wide as an ID with err. A name is
// what someone writing `mybox` most likely meant, and the candidates already
// ruled that out, so both readings are reported: only the ID one sends them
// looking for an ID problem they do not have.
func unmatchedSandboxReference(reference string, err error) error {
	return fmt.Errorf("no discobox for %q: none is named that, and %w", reference, err)
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
	// subcommands accept the same selectors as `admin box create --harness` and `prompt -a`.
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

// resolveProviderID accepts a provider instance's name, its ID, or a short ID.
// Nothing keeps two instances from sharing a name, so a shared one is refused
// as ambiguous rather than answered with one of them.
func (a *App) resolveProviderID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "provider ID")
	if err != nil || idpkg.IsGenerated(id) {
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
	providers := body.GetProviders()
	var named []string
	for _, provider := range providers {
		if provider.Name == value {
			named = append(named, provider.ID)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
	default:
		return "", fmt.Errorf("more than one provider is named %q (%s); give its ID", value, strings.Join(named, ", "))
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
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

// resolveSecretID accepts a secret's name, its ID, or a short ID. A name is
// what somebody reaches for, and it is how the secret is shown everywhere; it
// is unique only with the secret's type and host, so a name two secrets share
// is refused as ambiguous rather than answered with one of them.
func (a *App) resolveSecretID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "secret ID")
	if err != nil || idpkg.IsGenerated(id) {
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
	secrets := body.GetSecrets()
	var named []string
	for _, secret := range secrets {
		if secret.Name == value {
			named = append(named, secret.ID)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
	default:
		return "", fmt.Errorf("more than one secret is named %q (%s); give its ID", value, strings.Join(named, ", "))
	}
	if !isResolvableShortID(id) {
		return id, nil
	}
	ids := make([]string, 0, len(secrets))
	for _, secret := range secrets {
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
