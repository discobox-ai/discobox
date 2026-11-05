package secrets_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

var (
	readsRoute = model.SecretRoute{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}}
	pullsText  = "POST api.github.com/repos/acme/web/pulls"
)

// askToAdd files an ask for one more route in GH_TOKEN, in addition to what
// the discobox holds, for an hour.
func askToAdd(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Routes: []string{pullsText}, Add: serverapi.NewOptBool(true), GrantTTLSeconds: serverapi.NewOptInt64(3600),
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !req.Use.Add {
		t.Fatalf("request = %+v, want its use part asked as an addition", req)
	}
	return req
}

// approveAs approves the use part of a request, granted as as.
func approveAs(secretID string, as serverapi.SecretRequestPartDecisionAs) services.ApproveSecretRequestBody {
	return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secretID), Use: serverapi.NewOptSecretRequestPartDecision(serverapi.SecretRequestPartDecision{
		Decision: serverapi.SecretRequestPartDecisionDecisionApprove, As: serverapi.NewOptSecretRequestPartDecisionAs(as),
	})}
}

// deliverRow materializes the sandbox's row for env, under the binding that
// delivers it, as the sandbox service would.
func deliverRow(t *testing.T, st *store.Store, secretID, env, sentinel string) *model.SandboxSecret {
	t.Helper()
	ctx := context.Background()
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := st.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := effective.UseFor(env)
	if !ok {
		t.Fatalf("nothing delivers %s", env)
	}
	row := &model.SandboxSecret{ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secretID, EnvName: env, Sentinel: sentinel, BindingID: applied.Binding.ID}
	if err := st.CreateSandboxSecret(ctx, row); err != nil {
		t.Fatal(err)
	}
	return row
}

// The issue's case (ADR 26-11-03-329): a discobox holding a base of routes
// for good is granted one more route for an hour, as it asked. The base is
// untouched; the sentinel is held to both grants, and the value is handed out
// for as long as the base lasts; and once the addition lapses only its route
// stops being allowed, while the base's keep working.
func TestAnAdditionIsGrantedBesideTheBaseAndLapsesAlone(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	base := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{readsRoute}})
	row := deliverRow(t, st, secret.ID, "GH_TOKEN", "SENT-GH")
	req := askToAdd(ctx, t, svc)

	answered, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveUse(secret.ID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if answered.Use.ApprovedAs != model.SecretRequestApprovedAsAdd || answered.Use.BindingID == base.ID {
		t.Fatalf("request = %+v, want the use part granted as an addition of its own", answered.Use)
	}
	stored, err := st.SecretBindingByExactID(ctx, "project-1", base.ID)
	if err != nil || stored.ExpiresAt != nil || !reflect.DeepEqual(stored.Routes, base.Routes) {
		t.Fatalf("base = %+v, %v; want it untouched, for good", stored, err)
	}
	added, err := st.SecretBindingByExactID(ctx, "project-1", answered.Use.BindingID)
	if err != nil || !added.Adds || !added.Use || added.EnvName != "GH_TOKEN" || added.ExpiresAt == nil || time.Until(*added.ExpiresAt) > time.Hour {
		t.Fatalf("addition = %+v, %v; want an addition in GH_TOKEN for the hour asked", added, err)
	}

	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	pulls := secretroute.Request{Method: "POST", Host: "api.github.com", URL: "https://api.github.com/repos/acme/web/pulls", Body: func() ([]byte, bool) { return nil, true }}
	reads := secretroute.Request{Method: "GET", Host: "api.github.com", URL: "https://api.github.com/repos/acme/web/issues", Body: func() ([]byte, bool) { return nil, true }}
	grants, err := st.SentinelGrants(ctx, sandbox, []model.SandboxSecret{*row}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if held := grants["SENT-GH"]; len(held) != 2 || held[0].ExpiresAt != nil || held[1].ExpiresAt == nil {
		t.Fatalf("grants = %+v, want the base's for good and the addition's for its hour", held)
	}
	for name, request := range map[string]secretroute.Request{"the added route": pulls, "a base route": reads} {
		if allowed, reason := secretroute.DecideGrants(grants["SENT-GH"], time.Now(), request); !allowed {
			t.Fatalf("%s refused while both stand: %s", name, reason)
		}
	}
	resolved, err := svc.ResolveSandboxSecret(ctx, testPoolID, testSandboxID, "SENT-GH", "api.github.com")
	if err != nil || resolved.Status != model.SecretRequestStatusApproved || resolved.ExpiresAt != nil {
		t.Fatalf("resolve = %+v, %v; want the value, held for as long as the base", resolved, err)
	}
	listed, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(listed) != 1 || len(listed[0].Grants) != 2 || !listed[0].Grants[1].Adds || listed[0].ExpiresAt != nil {
		t.Fatalf("listed = %+v, %v; want one credential under two grants, lasting as the base does", listed, err)
	}

	// The addition lapses.
	lapsed := time.Now().UTC().Add(-time.Second)
	added.ExpiresAt = &lapsed
	if err := st.UpdateSecretBinding(ctx, added, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	grants, err = st.SentinelGrants(ctx, sandbox, []model.SandboxSecret{*row}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(grants["SENT-GH"]) != 1 {
		t.Fatalf("grants = %+v, want the base's alone once the addition lapsed", grants["SENT-GH"])
	}
	if allowed, _ := secretroute.DecideGrants(grants["SENT-GH"], time.Now(), pulls); allowed {
		t.Fatal("the lapsed addition's route was allowed")
	}
	if allowed, reason := secretroute.DecideGrants(grants["SENT-GH"], time.Now(), reads); !allowed {
		t.Fatalf("a base route was refused once the addition lapsed: %s", reason)
	}
	if stored, err = st.SecretBindingByExactID(ctx, "project-1", base.ID); err != nil || stored.ExpiresAt != nil {
		t.Fatalf("base = %+v, %v; want it standing, for good", stored, err)
	}
}

// An addition stacks on a base the discobox does not hold itself — its
// harness config's, or one handed on to it under a delegation — where a
// replacement would shadow the first and be refused over the second.
func TestAnAdditionStacksOnABaseTheDiscoboxDoesNotOwn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	delegation := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	base := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{readsRoute}, ParentID: delegation.ID})

	replacing := askToAdd(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", replacing.ID, approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsReplace)); !isStatus(err, http.StatusConflict) {
		t.Fatalf("replacing a base handed on under a delegation = %v, want 409", err)
	}
	answered, err := svc.ApproveSecretRequest(ctx, "project-1", replacing.ID, approveUse(secret.ID))
	if err != nil {
		t.Fatalf("a person adding beside a base handed on under a delegation: %v", err)
	}
	added, err := st.SecretBindingByExactID(ctx, "project-1", answered.Use.BindingID)
	if err != nil || !added.Adds || added.ParentID != "" {
		t.Fatalf("addition = %+v, %v; want a person's addition, under no delegation", added, err)
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := st.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if applied, ok := effective.UseFor("GH_TOKEN"); !ok || applied.Binding.ID != base.ID || len(applied.Additions) != 1 {
		t.Fatalf("GH_TOKEN = %+v, want the base still delivering it with the addition on it", applied)
	}
}

// A replacement states the discobox's whole use of the secret, so it clears
// the additions; and the approver may grant an ask either way (ADR
// 26-11-03-329 §4).
func TestAReplacementClearsAdditionsAndTheApproverChoosesTheWay(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	base := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{readsRoute}})
	asked := askToAdd(ctx, t, svc)
	added, err := svc.ApproveSecretRequest(ctx, "project-1", asked.ID, approveUse(secret.ID))
	if err != nil {
		t.Fatal(err)
	}

	// An ask to replace, granted as an addition.
	whole, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Routes: []string{"DELETE api.github.com/repos/acme/web/issues/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ApproveSecretRequest(ctx, "project-1", whole.ID, approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsAdd))
	if err != nil || second.Use.ApprovedAs != model.SecretRequestApprovedAsAdd {
		t.Fatalf("approve as add = %+v, %v; want a second addition", second, err)
	}

	// An ask to add, granted as a replacement: the base is kept and takes
	// it, and every addition goes.
	third := askToAdd(ctx, t, svc)
	replaced, err := svc.ApproveSecretRequest(ctx, "project-1", third.ID, approveAs(secret.ID, serverapi.SecretRequestPartDecisionAsReplace))
	if err != nil || replaced.Use.ApprovedAs != model.SecretRequestApprovedAsReplace || replaced.Use.BindingID != base.ID {
		t.Fatalf("approve as replace = %+v, %v; want the base kept and taking it", replaced, err)
	}
	ids := bindingIDs(t, st)
	if !ids[base.ID] || ids[added.Use.BindingID] || ids[second.Use.BindingID] {
		t.Fatalf("bindings = %v, want the base alone, every addition gone", ids)
	}
}

// What an addition may not be is refused before anything is written: the
// value itself, a delegate part granted as one, a variable another
// secret delivers, and an ask to add with nothing to use.
func TestWhatAnAdditionMayNotBeIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	other := createRoutedSecret(ctx, t, svc, "npm", "registry.npmjs.org", 0)

	if _, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Add: serverapi.NewOptBool(true), Delegate: delegateRoutes("api.github.com"),
	}); !isStatus(err, http.StatusBadRequest) {
		t.Errorf("an ask to add with no use part = %v, want 400", err)
	}
	both := askBoth(ctx, t, svc)
	asDelegate := services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Use: decide(true),
		Delegate: serverapi.NewOptSecretRequestPartDecision(serverapi.SecretRequestPartDecision{
			Decision: serverapi.SecretRequestPartDecisionDecisionApprove, As: serverapi.NewOptSecretRequestPartDecisionAs(serverapi.SecretRequestPartDecisionAsAdd),
		})}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", both.ID, asDelegate); !isStatus(err, http.StatusBadRequest) {
		t.Errorf("a delegate part granted as an addition = %v, want 400", err)
	}
	add := askToAdd(ctx, t, svc)
	raw := services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Use: decideRaw(serverapi.SecretRequestPartDecisionRawEnv)}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", add.ID, raw); !isStatus(err, http.StatusBadRequest) {
		t.Errorf("an addition of the value itself = %v, want 400", err)
	}
	mustBindAs(t, st, &model.SecretBinding{SecretID: other.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN"})
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", add.ID, approveUse(secret.ID)); !isStatus(err, http.StatusConflict) {
		t.Errorf("an addition where another secret is delivered = %v, want 409", err)
	}
	if got, err := st.GetSecretRequest(ctx, "project-1", add.ID); err != nil || got.Status != model.SecretRequestStatusPending {
		t.Fatalf("request = %+v, %v; want it still pending after every refusal", got, err)
	}
}

// An identical pending ask is reused only when it asks the same way.
func TestAnAskToAddIsNotTheSameAskAsOneToReplace(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	add := askToAdd(ctx, t, svc)
	if again := askToAdd(ctx, t, svc); again.ID != add.ID {
		t.Fatalf("the same ask to add was filed twice: %s and %s", add.ID, again.ID)
	}
	whole, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Routes: []string{pullsText}, GrantTTLSeconds: serverapi.NewOptInt64(3600),
	})
	if err != nil {
		t.Fatal(err)
	}
	if whole.ID == add.ID {
		t.Fatal("an ask to replace was answered with the pending ask to add")
	}
}

// An addition never stacks on a binding that hands over the value itself: the
// pool holds that value to its sentinel's grants, so an addition would widen
// where the value goes on what an approver read as a sentinel ask (ADR
// 26-11-03-329 Deferred). Approving one is a 409, and one written beside it
// some other way adds no grant to the raw sentinel.
func TestAnAdditionNeverWidensARawValue(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	allowRaw(ctx, t, svc, secret)
	raw, err := svc.CreateSecretBinding(ctx, "project-1", rawBindBody(serverapi.CreateSecretBindingBodyRawEnv))
	if err != nil {
		t.Fatalf("bind raw: %v", err)
	}
	req := askToAdd(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveUse(secret.ID)); !isStatus(err, http.StatusConflict) {
		t.Fatalf("an addition over a raw binding = %v, want 409", err)
	}
	if got, err := st.GetSecretRequest(ctx, "project-1", req.ID); err != nil || got.Status != model.SecretRequestStatusPending {
		t.Fatalf("request = %+v, %v; want it still pending", got, err)
	}

	hour := time.Now().UTC().Add(time.Hour)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, Adds: true, EnvName: "GH_TOKEN", Routes: []model.SecretRoute{{Hosts: []string{"attacker.example"}}}, ExpiresAt: &hour})
	row := &model.SandboxSecret{ProjectID: "project-1", SandboxID: testSandboxID, SecretID: secret.ID, EnvName: "GH_TOKEN", Sentinel: "RAW-GH", BindingID: raw.ID, Raw: raw.Raw}
	if err := st.CreateSandboxSecret(ctx, row); err != nil {
		t.Fatal(err)
	}
	sandbox, err := st.GetSandbox(ctx, "project-1", testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := st.SentinelGrants(ctx, sandbox, []model.SandboxSecret{*row}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(grants["RAW-GH"]) != 1 {
		t.Fatalf("raw sentinel grants = %+v, want its binding's alone", grants["RAW-GH"])
	}
}

// An addition that came first, under a base a person later turns raw, stops
// applying, with why, in every view: nothing stacks on a binding that hands
// over the value itself (ADR 26-11-03-329 Deferred).
func TestAnAdditionUnderABaseTurnedRawStopsApplying(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "api.github.com", 0)
	allowRaw(ctx, t, svc, secret)
	base, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
		Secret: "github", TargetKind: serverapi.CreateSecretBindingBodyTargetKindSandbox, Target: serverapi.NewOptString(testSandboxID), EnvVar: serverapi.NewOptString("GH_TOKEN"),
	})
	if err != nil {
		t.Fatal(err)
	}
	added, err := svc.ApproveSecretRequest(ctx, "project-1", askToAdd(ctx, t, svc).ID, approveUse(secret.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateSecretBinding(ctx, "project-1", base.ID, services.UpdateSecretBindingBody{Raw: serverapi.NewOptUpdateSecretBindingBodyRaw(serverapi.UpdateSecretBindingBodyRawEnv)}); err != nil {
		t.Fatalf("turn the base raw: %v", err)
	}
	bound, err := svc.ListBoundSecrets(ctx, "project-1", model.SecretBindingTargetSandbox, testSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, b := range bound {
		if b.Binding.ID == added.Use.BindingID {
			reason = b.NotApplied
		}
	}
	if !strings.Contains(reason, "hands over the value itself") {
		t.Fatalf("addition listed as %q, want it not applied beside a raw base", reason)
	}
	listed, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil || len(listed) != 1 || len(listed[0].Grants) != 1 {
		t.Fatalf("listed = %+v, %v; want the raw base's grant alone", listed, err)
	}
}
