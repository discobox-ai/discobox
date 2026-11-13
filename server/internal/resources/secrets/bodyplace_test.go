package secrets_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A discobox approving a body-place route under its delegation must not be
// told it succeeded when the grant it made can never carry the credential:
// every list a request is held to must name the body place, the secret's
// own included, and a secret routed to `*` names none. Either the approval
// is refused, saying so, or the grant it makes swaps the sentinel there.
func TestADelegatedBodyPlaceApprovalIsNeverAGrantThatAllowsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent []model.SecretRoute
	}{
		{"delegation routed like the secret", []model.SecretRoute{{Hosts: []string{"*"}}}},
		{"delegation inheriting the secret's routes", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createRoutedSecret(ctx, t, svc, "boxd", "*", 0)
			mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, Routes: tc.parent})

			exchange, err := secretroute.Parse("POST app.boxd.sh/api/v1/auth/token body=/api_key")
			if err != nil {
				t.Fatal(err)
			}
			routes, err := services.Convert[[]apimodel.SecretRouteInput]([]model.SecretRoute{exchange})
			if err != nil {
				t.Fatal(err)
			}
			req := createAgentRequest(ctx, t, svc)
			decision := apigen.SecretRequestPartDecision{Decision: apigen.SecretRequestPartDecisionDecisionApprove, Routes: routes}
			answer, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secret.ID), Use: apigen.NewOptSecretRequestPartDecision(decision)})
			if err != nil {
				return // refused at approval: the approver is told
			}
			lists := answer.Use.Receipt.RouteLists
			allowed, reason := secretroute.Decide(lists, secretroute.Request{
				Method: http.MethodPost, Host: "app.boxd.sh", URL: "/api/v1/auth/token",
				Header: http.Header{"Content-Type": {"application/json"}},
				At:     []string{"/api_key"},
			})
			if !allowed {
				t.Fatalf("approval succeeded, but its grant %v refuses the credential at the body place it names: %s", lists, reason)
			}
		})
	}
}

const (
	exchangeText = "POST app.boxd.sh/api/v1/auth/token body=/api_key"
	boxdAPIText  = "POST boxd.sh,fra.boxd.sh,nyc.boxd.sh/boxd.api.v1.BoxdApi/**"
)

// typedRoutes reads routes in their typed form, as an approver's input.
func typedRoutes(t *testing.T, texts ...string) []apimodel.SecretRouteInput {
	t.Helper()
	var parsed []model.SecretRoute
	for _, text := range texts {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, route)
	}
	routes, err := services.Convert[[]apimodel.SecretRouteInput](parsed)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

// createBoxdKey stores a key routed to texts, as a person would.
func createBoxdKey(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, texts ...string) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "boxd",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString("boxd_realrealrealrealrealrealreal12")},
		Routes: apigen.NewOptNilSecretRouteInputArray(typedRoutes(t, texts...)),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// approveRoutesAs approves a request's use part as the lead, for routes,
// granted as as.
func approveRoutesAs(t *testing.T, svc *resourcesecrets.Service, requestID, secretID string, as apigen.SecretRequestPartDecisionAs, texts ...string) (*model.SecretRequest, error) {
	t.Helper()
	decision := apigen.SecretRequestPartDecision{Decision: apigen.SecretRequestPartDecisionDecisionApprove, Routes: typedRoutes(t, texts...), As: apigen.NewOptSecretRequestPartDecisionAs(as)}
	return svc.ApproveSecretRequest(asLead(), "project-1", requestID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secretID), Use: apigen.NewOptSecretRequestPartDecision(decision)})
}

// The issue's grant, once the key itself names the exchange's body place: a
// discobox approves it under its delegation, first wide and then as a
// replacement with only the two narrow routes. The sentinel is held to the
// replacement alone — no wider route is left behind — and the proxy's
// decision swaps it at the exchange's body place and nowhere else.
func TestADelegatedBodyPlaceGrantSwapsAtItsPlaceAndAReplacementLeavesNothingWider(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoxdKey(ctx, t, svc, "*", exchangeText)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})

	wide, err := approveRoutesAs(t, svc, createAgentRequest(ctx, t, svc).ID, secret.ID, apigen.SecretRequestPartDecisionAsReplace, "*", exchangeText)
	if err != nil {
		t.Fatalf("approve wide: %v", err)
	}
	narrow, err := approveRoutesAs(t, svc, createAgentRequest(ctx, t, svc).ID, secret.ID, apigen.SecretRequestPartDecisionAsReplace, exchangeText, boxdAPIText)
	if err != nil {
		t.Fatalf("approve the replacement: %v", err)
	}
	if narrow.Use.ApprovedAs != model.SecretRequestApprovedAsReplace {
		t.Fatalf("request = %+v, want the use part granted as a replacement", narrow.Use)
	}
	ids := bindingIDs(t, st)
	if wide.Use.BindingID != narrow.Use.BindingID && ids[wide.Use.BindingID] {
		t.Fatalf("bindings = %v, want the wide grant replaced", ids)
	}
	stored, err := st.SecretBindingByExactID(ctx, "project-1", narrow.Use.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	var want []model.SecretRoute
	for _, text := range []string{exchangeText, boxdAPIText} {
		route, _ := secretroute.Parse(text)
		want = append(want, route)
	}
	if !reflect.DeepEqual(formatAll(stored.Routes), formatAll(want)) {
		t.Fatalf("binding routes = %v, want the two approved", formatAll(stored.Routes))
	}

	row := deliverRow(t, st, secret.ID, "GITHUB_TOKEN", "SENT-BOXD")
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := st.SentinelGrants(ctx, sandbox, []model.SandboxSecret{*row}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(grants["SENT-BOXD"]) != 1 {
		t.Fatalf("grants = %+v, want the replacement's alone", grants["SENT-BOXD"])
	}
	json := http.Header{"Content-Type": {"application/json"}}
	exchange := func(at ...string) secretroute.Request {
		return secretroute.Request{Method: http.MethodPost, Host: "app.boxd.sh", URL: "https://app.boxd.sh/api/v1/auth/token", Header: json, At: at}
	}
	for name, tc := range map[string]struct {
		req     secretroute.Request
		allowed bool
	}{
		"the exchange at its body place":      {exchange("/api_key"), true},
		"the exchange at another body place":  {exchange("/token"), false},
		"the exchange in the headers":         {exchange(), false},
		"the boxd API in the headers":         {secretroute.Request{Method: http.MethodPost, Host: "fra.boxd.sh", URL: "https://fra.boxd.sh/boxd.api.v1.BoxdApi/ListBoxes", Header: json}, true},
		"another host the wide grant allowed": {secretroute.Request{Method: http.MethodGet, Host: "api.github.com", URL: "https://api.github.com/user"}, false},
		"the body place on another host":      {secretroute.Request{Method: http.MethodPost, Host: "evil.example", URL: "https://evil.example/api/v1/auth/token", Header: json, At: []string{"/api_key"}}, false},
	} {
		if allowed, reason := secretroute.DecideGrants(grants["SENT-BOXD"], time.Now(), tc.req); allowed != tc.allowed {
			t.Errorf("%s: allowed = %v (%s), want %v", name, allowed, reason, tc.allowed)
		}
	}
}

// formatAll is routes in their typed form.
func formatAll(routes []model.SecretRoute) []string {
	var out []string
	for _, r := range routes {
		out = append(out, secretroute.Format(r))
	}
	return out
}

// A grant naming a body place a secret routed to `*` never names is refused
// at every door that writes one, saying the secret needs that route: a
// person's binding, its update, a create's binding, a person's approval and
// a discobox's.
func TestABodyPlaceTheSecretNeverNamesIsRefusedAtEveryDoor(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "boxd", "*", 0)
	delegation := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	routes := typedRoutes(t, exchangeText)
	refused := func(door string, err error) {
		t.Helper()
		if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "body place /api_key") || !strings.Contains(err.Error(), "secret boxd's routes") {
			t.Errorf("%s = %v, want 400 naming the body place and the secret's routes", door, err)
		}
	}

	_, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
		Secret: "boxd", TargetKind: apigen.CreateSecretBindingBodyTargetKindSandbox, Target: apigen.NewOptString(testSandboxID), EnvVar: apigen.NewOptString("BOXD_KEY"), Routes: routes,
	})
	refused("a person's binding", err)

	bound, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
		Secret: "boxd", TargetKind: apigen.CreateSecretBindingBodyTargetKindSandbox, Target: apigen.NewOptString(testSandboxID), EnvVar: apigen.NewOptString("BOXD_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpdateSecretBinding(ctx, "project-1", bound.ID, services.UpdateSecretBindingBody{Routes: routes})
	refused("a person's update", err)

	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	for who, as := range map[string]context.Context{"a person's create": ctx, "a discobox's create": asLead()} {
		_, err = svc.PrepareSandboxBindings(as, "project-1", sandbox, []apimodel.SandboxBindingInput{{Secret: "boxd", EnvVar: apigen.NewOptString("OTHER_KEY"), Routes: routes}})
		refused(who, err)
	}

	decision := apigen.SecretRequestPartDecision{Decision: apigen.SecretRequestPartDecisionDecisionApprove, Routes: routes}
	body := services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(secret.ID), Use: apigen.NewOptSecretRequestPartDecision(decision)}
	for who, as := range map[string]context.Context{"a person's approval": ctx, "a discobox's approval": asLead()} {
		_, err = svc.ApproveSecretRequest(as, "project-1", createAgentRequest(ctx, t, svc).ID, body)
		refused(who, err)
	}

	// The issue's own delegation, routed `*` like the secret, refuses the
	// same way and says why.
	delegation.Routes = []model.SecretRoute{{Hosts: []string{"*"}}}
	if err := st.UpdateSecretBinding(context.Background(), delegation, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApproveSecretRequest(asLead(), "project-1", createAgentRequest(ctx, t, svc).ID, body)
	refused("a discobox's approval under a delegation routed *", err)
}

// A binding handed on under a delegation is held to the delegation's routes
// too: a person narrowing it to a body place the delegation never names is
// refused, naming the delegation.
func TestABodyPlaceTheDelegationNeverNamesIsRefusedOnUpdate(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBoxdKey(ctx, t, svc, "*", exchangeText)
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, Routes: []model.SecretRoute{{Hosts: []string{"*"}}}})
	child := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "BOXD_KEY", ParentID: parent.ID})
	_, err := svc.UpdateSecretBinding(ctx, "project-1", child.ID, services.UpdateSecretBindingBody{Routes: typedRoutes(t, exchangeText)})
	if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "delegation") || !strings.Contains(err.Error(), "body place /api_key") {
		t.Fatalf("update = %v, want 400 naming the delegation and the body place", err)
	}
}
