package secrets_test

import (
	"net/http"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A person editing a secret's routes must not strand, silently, a standing
// binding whose body place the new routes allow nowhere: every list a request
// is held to must name the body place, the secret's own included. Either the
// edit is refused, or the binding it strands is still one the proxy honors.
func TestASecretRouteEditNeverSilentlyStrandsABodyPlaceBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	exchange, err := secretroute.Parse("POST app.boxd.sh/api/v1/auth/token body=/api_key")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "boxd",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString("boxd-real-key")},
		Routes: apigen.NewOptNilSecretRouteInputArray([]apigen.SecretRouteInput{{Hosts: []string{"*"}}, {Hosts: exchange.Hosts, Methods: exchange.Methods, Paths: exchange.Paths, Body: apigen.NewOptSecretRouteBody(apigen.SecretRouteBody{Places: exchange.Body.Places})}}),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	binding := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "BOXD_KEY", Routes: []model.SecretRoute{exchange}})

	exchangeAllowed := func(secret *model.Secret) (bool, string) {
		return secretroute.Decide(store.RouteLists(secret, binding, nil), secretroute.Request{
			Method: http.MethodPost, Host: "app.boxd.sh", URL: "https://app.boxd.sh/api/v1/auth/token",
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   func() ([]byte, bool) { return []byte(`{"api_key":"SENTINEL"}`), true },
			At:     []string{"/api_key"},
		})
	}
	if ok, why := exchangeAllowed(secret); !ok {
		t.Fatalf("before the edit the binding should swap at /api_key: %s", why)
	}

	// The edit drops the body-place route, leaving the secret routed to `*`.
	edited, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: hostRoutes("*")})
	if err != nil {
		return // refused: the person is told
	}
	if _, err := st.GetSecretBinding(ctx, "project-1", binding.ID); err != nil {
		return // the stranded binding went with the edit
	}
	if ok, why := exchangeAllowed(edited); !ok {
		t.Fatalf("the route edit was accepted and binding %s still stands as granted, but the proxy refuses it on every use: %s", binding.ID, why)
	}
}
