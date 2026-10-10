package secrets_test

import (
	"context"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type recordedPushes struct{ secretIDs []string }

func (r *recordedPushes) PushSecretSentinels(_ context.Context, _, secretID string) error {
	r.secretIDs = append(r.secretIDs, secretID)
	return nil
}

func routeInput(host, path, use string) apigen.SecretRouteInput {
	in := apigen.SecretRouteInput{Host: host}
	if path != "" {
		in.Path = apigen.NewOptString(path)
	}
	if use != "" {
		in.Use = apigen.NewOptString(use)
	}
	return in
}

func routes(in ...apigen.SecretRouteInput) apigen.OptNilSecretRouteInputArray {
	return apigen.NewOptNilSecretRouteInputArray(in)
}

// secretRouteFixture is a sandbox holding an injected secret that carries
// routes (ADR 26-10-02-924): one sent unjudged, two judged.
func secretRouteFixture(t *testing.T) (*resourcesecrets.Service, *store.Store, *model.Secret) {
	t.Helper()
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	sec, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "copilot",
		Type:  apigen.CreateSecretBodyTypeToken,
		Value: apigen.SecretValue{Token: apigen.NewOptString("real-token")},
		Routes: routes(
			routeInput("GitHubCopilot.com", "/", ""),
			routeInput("githubcopilot.com", "/mcp/", "Copilot's own MCP traffic"),
			routeInput("github.com", "", "reading public GitHub data"),
		),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	createSandboxWithHarness(t, st, "sb-1", "pool-1", "")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-S")
	return svc, st, sec
}

func sentinelRoutes(t *testing.T, st *store.Store, judge bool) map[string][]secretroute.Route {
	t.Helper()
	ctx := context.Background()
	sandbox, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if judge {
		sandbox.HarnessMode = sandboxconfig.HarnessModeJudge
	}
	assignments, err := st.ListInjectedSandboxSecrets(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := st.SecretSentinelRoutes(ctx, sandbox, assignments)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestASecretsRoutesAreNormalizedAndOnlyUsesAreMinted(t *testing.T) {
	_, _, sec := secretRouteFixture(t)
	if len(sec.Routes) != 3 {
		t.Fatalf("routes = %+v, want three", sec.Routes)
	}
	if got := sec.Routes[0]; got.Host != "githubcopilot.com" || got.Path != "" || got.UseID != "" {
		t.Fatalf("route = %+v, want the whole host, normalized, with no use ID", got)
	}
	if got := sec.Routes[1]; got.Path != "/mcp" || got.UseID == "" {
		t.Fatalf("route = %+v, want its trailing slash trimmed and a use ID minted", got)
	}
	if sec.Routes[1].UseID == sec.Routes[2].UseID {
		t.Fatalf("routes = %+v, want distinct use IDs", sec.Routes)
	}
}

func TestASecretsRoutesKeepTheirUseIDsAcrossEdits(t *testing.T) {
	ctx := context.Background()
	svc, _, sec := secretRouteFixture(t)
	pushes := &recordedPushes{}
	svc.SetSentinelPusher(pushes)
	kept := sec.Routes[1]

	updated, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Routes: routes(
			routeInput(kept.Host, kept.Path, kept.Use),
			routeInput(kept.Host, kept.Path, kept.Use),
			routeInput("github.com", "", "opening pull requests"),
		),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(updated.Routes) != 2 || updated.Routes[0] != kept || updated.Routes[1].UseID == sec.Routes[2].UseID {
		t.Fatalf("routes = %+v, want the unchanged one kept, the repeat dropped and the new one minted", updated.Routes)
	}
	if len(pushes.secretIDs) != 1 || pushes.secretIDs[0] != sec.ID {
		t.Fatalf("pushes = %v, want the change carried to the discoboxes holding it", pushes.secretIDs)
	}

	// An edit that leaves the routes alone carries nothing.
	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Name: apigen.NewOptString("renamed")}); err != nil {
		t.Fatal(err)
	}
	if len(pushes.secretIDs) != 1 {
		t.Fatalf("pushes = %v, want none for an edit that left the routes alone", pushes.secretIDs)
	}

	cleared, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Routes: routes()})
	if err != nil || len(cleared.Routes) != 0 {
		t.Fatalf("routes = %+v, err = %v, want an empty list to clear them", cleared.Routes, err)
	}

	for _, bad := range []apigen.SecretRouteInput{routeInput(" ", "", "a use"), routeInput("github.com", "mcp", "")} {
		if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Routes: routes(bad)}); err == nil {
			t.Fatalf("update with route %+v succeeded, want it refused", bad)
		}
	}
}

// Every request an injected sentinel is sent in must fall on its secret's
// routes: the pool is told them, and ApprovedUse answers for a route's use.
func TestAnInjectedSecretIsHeldToItsRoutes(t *testing.T) {
	ctx := context.Background()
	svc, st, sec := secretRouteFixture(t)

	got := sentinelRoutes(t, st, false)["SENTINEL-S"]
	if len(got) != 3 || got[0] != (secretroute.Route{Host: "githubcopilot.com"}) ||
		got[2] != (secretroute.Route{Host: "github.com", UseID: sec.Routes[2].UseID}) {
		t.Fatalf("sentinel routes = %+v, want the secret's, by use ID", got)
	}

	use, err := svc.ApprovedUse(ctx, "pool-1", "sb-1", sec.Routes[2].UseID, "api.github.com")
	if err != nil {
		t.Fatalf("ApprovedUse() error = %v", err)
	}
	if use.Purpose != "reading public GitHub data" || use.Credential != "copilot" || use.Host != "github.com" || use.GrantID != "" {
		t.Fatalf("use = %+v, want the route's use, for its host, under no grant", use)
	}
	if _, err := svc.ApprovedUse(ctx, "pool-1", "sb-1", sec.Routes[2].UseID, "example.com"); err == nil {
		t.Fatal("ApprovedUse() succeeded for a host the route does not cover")
	}

	// The agent cannot run a command under it: it is not a grant's use.
	if _, err := svc.ApprovedCredentialUse(ctx, "pool-1", "sb-1", sec.Routes[1].UseID); err == nil {
		t.Fatal("ApprovedCredentialUse() succeeded, want a route's use not to be one an agent runs under")
	}
}

// A judge discobox's own credentials are held to none: its requests would
// wait on its own verdict.
func TestAJudgesSecretsAreNotHeldToTheirRoutes(t *testing.T) {
	ctx := context.Background()
	svc, st, sec := secretRouteFixture(t)
	if got := sentinelRoutes(t, st, true); len(got) != 0 {
		t.Fatalf("sentinel routes = %+v, want none for a judge", got)
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	sandbox.HarnessMode = sandboxconfig.HarnessModeJudge
	if err := st.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApprovedUse(ctx, "pool-1", "sb-1", sec.Routes[1].UseID, "api.githubcopilot.com"); err == nil {
		t.Fatal("ApprovedUse() succeeded for a judge's own secret")
	}
}

// An agent-requested credential carries its secret's routes with it, so an
// activation is held to them as well as to its grant's use.
func TestAnAgentCredentialCarriesItsSecretsRoutes(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	sec, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "github",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString("real-token")},
		Routes: routes(routeInput("github.com", "", "reading public GitHub data")),
	})
	if err != nil {
		t.Fatal(err)
	}
	createSandboxWithHarness(t, st, "sb-1", "pool-1", "")
	grant := &model.SecretGrant{
		ProjectID: "project-1", SecretID: sec.ID, Scope: model.SecretGrantScopeSandbox, ScopeKey: "sb-1",
		EnvName: "GITHUB_TOKEN", Uses: []model.SecretUse{{UseID: "use_grant", Description: "open a pull request"}},
	}
	if err := st.CreateSecretGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := st.ListLiveAgentCredentials(ctx, "project-1", "sb-1", store.SandboxGrantScopes(sandbox))
	if err != nil || len(credentials) != 1 || len(credentials[0].SecretRoutes) != 1 {
		t.Fatalf("credentials = %+v, err = %v, want one carrying its secret's route", credentials, err)
	}
	use, err := svc.ApprovedUse(ctx, "pool-1", "sb-1", sec.Routes[0].UseID, "api.github.com")
	if err != nil || use.GrantID != grant.ID || use.Purpose != "reading public GitHub data" {
		t.Fatalf("use = %+v, err = %v, want the route's use under the grant it was handed out by", use, err)
	}
}
