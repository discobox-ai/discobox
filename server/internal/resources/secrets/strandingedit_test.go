package secrets_test

import (
	"net/http"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A person's route edit that strands a standing body-place binding is
// refused, 409, naming the binding and the body place, and changes nothing.
// An edit that keeps the place, strands only a binding the routes had
// stranded already, or follows routes that sent the credential nowhere, goes
// through (#1105).
func TestARouteEditThatStrandsABodyPlaceBindingIsRefusedNamingIt(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	exchange := mustParseRoute(t, exchangeText)
	routed := func(texts ...string) apigen.OptNilSecretRouteInputArray {
		return apigen.NewOptNilSecretRouteInputArray(typedRoutes(t, texts...))
	}
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "boxd",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString("boxd-real-key")},
		Routes: routed("*", exchangeText),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "BOXD_KEY", Routes: []model.SecretRoute{exchange}})

	_, err = svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: routed("*")})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), binding.ID) || !strings.Contains(err.Error(), "body place /api_key") {
		t.Fatalf("update = %v, want 409 naming binding %s and the body place", err, binding.ID)
	}
	if got, err := st.GetSecret(ctx, "project-1", secret.ID); err != nil || len(got.Routes) != 2 {
		t.Fatalf("a refused edit changed the routes: %+v, %v", got, err)
	}

	// Narrowing to the exchange alone keeps the place: allowed.
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: routed("POST app.boxd.sh/** body=/api_key")}); err != nil {
		t.Fatalf("an edit that keeps the body place was refused: %v", err)
	}

	// A binding the routes already strand (written past the check) is not the
	// next edit's to answer for.
	stale := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "OTHER_KEY", Routes: []model.SecretRoute{mustParseRoute(t, "POST app.boxd.sh/** body=/token")}})
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: routed("*", "POST app.boxd.sh/** body=/api_key")}); err != nil {
		t.Fatalf("binding %s, stranded before the edit, refused it: %v", stale.ID, err)
	}

	// Removing every route sends the credential nowhere, deliberately: no
	// binding is stranded by a body place, and the edit goes through.
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: apigen.NewOptNilSecretRouteInputArray([]apigen.SecretRouteInput{})}); err != nil {
		t.Fatalf("an edit sending the credential nowhere was refused: %v", err)
	}
	// A secret routed nowhere sent the binding nothing before, so routes that
	// still leave its body place out strand nothing either.
	if _, err := svc.UpdateSecret(ctx, "project-1", secret.ID, services.UpdateSecretBody{Routes: routed("*")}); err != nil {
		t.Fatalf("giving a route-less secret its first routes was refused for a binding it never served: %v", err)
	}
}

func mustParseRoute(t *testing.T, text string) model.SecretRoute {
	t.Helper()
	route, err := secretroute.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return route
}
