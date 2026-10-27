package secrets_test

import (
	"context"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func routeInput(host, path string, blocked bool) apigen.SecretRouteInput {
	in := apigen.SecretRouteInput{Host: host}
	if path != "" {
		in.Path = apigen.NewOptString(path)
	}
	if blocked {
		in.Blocked = apigen.NewOptBool(true)
	}
	return in
}

func routes(in ...apigen.SecretRouteInput) apigen.OptNilSecretRouteInputArray {
	return apigen.NewOptNilSecretRouteInputArray(in)
}

// secretRouteFixture is a sandbox holding an injected secret that carries
// routes (ADR 26-10-02-924): one open, two blocked.
func secretRouteFixture(t *testing.T) (*resourcesecrets.Service, *store.Store, *model.Secret) {
	t.Helper()
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	sec, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "copilot",
		Type:  apigen.CreateSecretBodyTypeToken,
		Value: apigen.SecretValue{Token: apigen.NewOptString("real-token")},
		Routes: routes(
			routeInput("GitHubCopilot.com", "/", false),
			routeInput("githubcopilot.com", "/mcp/", true),
			routeInput("github.com", "", true),
		),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	createSandboxWithHarness(t, st, "sb-1", "pool-1", "")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-S")
	return svc, st, sec
}

func sentinelRoutes(t *testing.T, st *store.Store) map[string][]secretroute.Route {
	t.Helper()
	ctx := context.Background()
	assignments, err := st.ListSandboxSecrets(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := st.SecretSentinelRoutes(ctx, "project-1", assignments)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestASecretsRoutesAreNormalized(t *testing.T) {
	_, _, sec := secretRouteFixture(t)
	want := []model.SecretRoute{
		{Host: "githubcopilot.com"},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "github.com", Blocked: true},
	}
	if len(sec.Routes) != len(want) {
		t.Fatalf("routes = %+v, want %+v", sec.Routes, want)
	}
	for i := range want {
		if sec.Routes[i] != want[i] {
			t.Fatalf("routes = %+v, want %+v", sec.Routes, want)
		}
	}
}

// A change to a secret's routes marks every discobox holding it, so each is
// sent the routes as written; an edit that leaves them alone marks nothing.
func TestAChangeToASecretsRoutesReachesItsHolders(t *testing.T) {
	ctx := context.Background()
	svc, _, sec := secretRouteFixture(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)

	updated, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Routes: routes(
			routeInput("githubcopilot.com", "/mcp", true),
			routeInput("githubcopilot.com", "/mcp", true),
			routeInput("github.com", "", false),
		),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(updated.Routes) != 2 || updated.Routes[1] != (model.SecretRoute{Host: "github.com"}) {
		t.Fatalf("routes = %+v, want the repeat dropped and github.com unblocked", updated.Routes)
	}
	if len(marks.secrets) != 1 || marks.secrets[0] != sec.ID {
		t.Fatalf("marks = %v, want the change to mark the discoboxes holding it for reconcile", marks.secrets)
	}

	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Name: apigen.NewOptString("renamed")}); err != nil {
		t.Fatal(err)
	}
	if len(marks.secrets) != 1 {
		t.Fatalf("marks = %v, want none for an edit that left the routes alone", marks.secrets)
	}

	cleared, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Routes: routes()})
	if err != nil || len(cleared.Routes) != 0 {
		t.Fatalf("routes = %+v, err = %v, want an empty list to clear them", cleared.Routes, err)
	}

	for _, bad := range []apigen.SecretRouteInput{routeInput(" ", "", false), routeInput("github.com", "mcp", false)} {
		if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Routes: routes(bad)}); err == nil {
			t.Fatalf("update with route %+v succeeded, want it refused", bad)
		}
	}
}

// Every request an injected sentinel is sent in must fall on its secret's
// routes: the pool is told them, blocked ones included.
func TestAnInjectedSecretIsHeldToItsRoutes(t *testing.T) {
	_, st, _ := secretRouteFixture(t)
	got := sentinelRoutes(t, st)["SENTINEL-S"]
	want := []secretroute.Route{
		{Host: "githubcopilot.com"},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "github.com", Blocked: true},
	}
	if len(got) != len(want) {
		t.Fatalf("sentinel routes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sentinel routes = %+v, want %+v", got, want)
		}
	}
}
