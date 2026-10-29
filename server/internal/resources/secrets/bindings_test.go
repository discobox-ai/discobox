package secrets_test

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A binding is made by a person, named by name, shaped for what it does, and
// held to its secret's limit (ADR 26-10-27-880 §1).
func TestCreateSecretBindingIsShapedAndLimited(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "github.com", 3600)
	sandboxTarget := serverapi.NewOptString("sandbox")
	bind := func(body services.CreateSecretBindingBody) (*model.SecretBinding, error) {
		if body.Secret == "" {
			body.Secret = "github"
		}
		if body.TargetKind == "" {
			body.TargetKind, body.Target = serverapi.CreateSecretBindingBodyTargetKindSandbox, sandboxTarget
		}
		return svc.CreateSecretBinding(ctx, "project-1", body)
	}
	for name, tc := range map[string]struct {
		body services.CreateSecretBindingBody
		want int
	}{
		"a use with no variable":           {services.CreateSecretBindingBody{}, http.StatusBadRequest},
		"a delegation naming a variable":   {services.CreateSecretBindingBody{Delegate: serverapi.NewOptBool(true), EnvVar: serverapi.NewOptString("X")}, http.StatusBadRequest},
		"longer than the secret allows":    {services.CreateSecretBindingBody{EnvVar: serverapi.NewOptString("X"), TtlSeconds: serverapi.NewOptInt64(7200)}, http.StatusBadRequest},
		"forever under a limit":            {services.CreateSecretBindingBody{EnvVar: serverapi.NewOptString("X"), TtlSeconds: serverapi.NewOptInt64(0)}, http.StatusBadRequest},
		"a secret the project has not got": {services.CreateSecretBindingBody{Secret: "nope", EnvVar: serverapi.NewOptString("X")}, http.StatusNotFound},
	} {
		if _, err := bind(tc.body); !isStatus(err, tc.want) {
			t.Errorf("%s = %v, want %d", name, err, tc.want)
		}
	}
	binding, err := bind(services.CreateSecretBindingBody{EnvVar: serverapi.NewOptString("GH_TOKEN"), Routes: []serverapi.SecretRouteInput{{Hosts: []string{"api.github.com"}}}})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if binding.TargetID != testSandboxID || !binding.Use || binding.SecretName != "github" || binding.CreatedBy != "user-1" || binding.ExpiresAt == nil ||
		time.Until(*binding.ExpiresAt) > time.Hour || !reflect.DeepEqual(binding.Routes, []model.SecretRoute{{Hosts: []string{"api.github.com"}}}) {
		t.Fatalf("binding = %+v, want a use on the sandbox lasting the secret's hour", binding)
	}
	if _, err := bind(services.CreateSecretBindingBody{EnvVar: serverapi.NewOptString("GH_TOKEN")}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("a second binding of the variable = %v, want 409", err)
	}
	if _, err := svc.CreateSecretBinding(asLead(), "project-1", services.CreateSecretBindingBody{
		Secret: secret.ID, TargetKind: serverapi.CreateSecretBindingBodyTargetKindProject, EnvVar: serverapi.NewOptString("Y"),
	}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a discobox's binding = %v, want 403", err)
	}
}

// Removing a binding removes what was handed on under it, and marks the
// discoboxes both covered; the history says why each went.
func TestDeletingABindingTakesWhatWasHandedOnUnderIt(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	child := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH", ParentID: parent.ID})

	deleted, err := svc.DeleteSecretBinding(ctx, "project-1", parent.ID)
	if err != nil || !reflect.DeepEqual(deleted, []string{parent.ID, child.ID}) {
		t.Fatalf("deleted = %v, %v; want the parent then its child", deleted, err)
	}
	if len(marks.bindings) != 2 {
		t.Fatalf("marks = %+v, want both bindings' holders", marks.bindings)
	}
	events, err := svc.ListSecretBindingEvents(ctx, "project-1", services.SecretBindingEventQuery{Secret: "github"})
	if err != nil || len(events) != 4 || events[0].Cause != parent.ID && events[1].Cause != parent.ID {
		t.Fatalf("events = %+v, %v; want each creation and removal, the child's caused by its parent", events, err)
	}
}

// The resolved view of a discobox: per variable the narrowest binding, with
// the route lists it is held to, and every delegation.
func TestBoundSecretsResolveTheNarrowest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "GH"})
	own := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH",
		Routes: []model.SecretRoute{{Hosts: []string{"api.github.com"}}}})
	delegate := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Delegate: true})

	bound, err := svc.ListBoundSecrets(ctx, "project-1", model.SecretBindingTargetSandbox, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 2 || bound[0].Binding.ID != own.ID || bound[1].Binding.ID != delegate.ID || bound[0].SecretName != "github" ||
		!reflect.DeepEqual(bound[0].RouteLists, [][]model.SecretRoute{{{Hosts: []string{"github.com"}}}, {{Hosts: []string{"api.github.com"}}}}) {
		t.Fatalf("bound = %+v, want its own GH over the project's, and the project's delegation", bound)
	}
}

// The sweep removes a lapsed binding and what was handed on under it, records
// each as expired, marks their holders, and arms itself for the next.
func TestTheExpirySweepRemovesLapsedBindings(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	lapsed, later := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(time.Hour)
	gone := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "A", ExpiresAt: &lapsed})
	kept := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "B", ExpiresAt: &later})

	result, err := svc.NewBindingExpiryReconciler().Reconcile(context.Background(), store.BindingExpiryID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RequeueAt.Equal(later) {
		t.Fatalf("requeue at %v, want the next expiry %v", result.RequeueAt, later)
	}
	if _, err := st.SecretBindingByExactID(ctx, "project-1", gone.ID); err == nil {
		t.Fatal("the lapsed binding is still there")
	}
	if _, err := st.SecretBindingByExactID(ctx, "project-1", kept.ID); err != nil {
		t.Fatalf("the live binding went: %v", err)
	}
	// Created and expired can share a clock tick (Windows), which leaves
	// their order in the listing open: look for the expiry, not its place.
	events, err := st.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: gone.ID})
	expired := slices.IndexFunc(events, func(e model.SecretBindingEvent) bool {
		return e.Event == model.SecretBindingEventExpired && e.Actor == model.SecretBindingActorSystem
	})
	if err != nil || len(events) != 2 || expired < 0 {
		t.Fatalf("events = %+v, %v; want the expiry recorded as the system's", events, err)
	}
	if len(marks.bindings) != 1 || marks.bindings[0].ID != gone.ID {
		t.Fatalf("marks = %+v, want the lapsed binding's holders", marks.bindings)
	}
}

// A discobox creating another binds on it only what it was delegated, for
// use, handed on under its delegate binding and held to it again when stored.
func TestACreatesBindingsAreHeldToTheirDelegation(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	newBox := &model.Sandbox{ID: "sbx-new", ProjectID: "project-1"}
	input := []apimodel.SandboxBindingInput{{Secret: "github", EnvVar: serverapi.NewOptString("GH")}}
	if _, err := svc.PrepareSandboxBindings(asLead(), "project-1", newBox, input); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("prepare with nothing delegated = %v, want 403", err)
	}
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	if _, err := svc.PrepareSandboxBindings(asLead(), "project-1", newBox, []apimodel.SandboxBindingInput{{Secret: "github", Delegate: serverapi.NewOptBool(true)}}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a discobox delegating = %v, want 403", err)
	}
	prepared, err := svc.PrepareSandboxBindings(asLead(), "project-1", newBox, input)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(prepared.Bindings) != 1 || prepared.Bindings[0].ParentID != parent.ID || prepared.Bindings[0].CreatedBy != leadID {
		t.Fatalf("prepared = %+v, want a child of the lead's delegation", prepared.Bindings)
	}
	if _, err := svc.DeleteSecretBinding(ctx, "project-1", parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := resourcesecrets.StoreSandboxBindingsTx(ctx, st, prepared); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("store after the delegation went = %v, want 403", err)
	}
}

// A refresh request is listed with its secret's name, and dismissed once.
func TestARefreshRequestIsListedAndDismissed(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := &model.SecretRefreshRequest{ProjectID: "project-1", SecretID: secret.ID, SandboxID: testSandboxID, Cause: model.SecretRefreshCauseStale, Status: model.SecretRefreshStatusPending}
	if err := st.CreateSecretRefreshRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.ListSecretRefreshRequests(ctx, "project-1", model.SecretRefreshStatusPending)
	if err != nil || len(listed) != 1 || listed[0].SecretName != "github" {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
	if _, err := svc.DismissSecretRefreshRequest(ctx, "project-1", req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DismissSecretRefreshRequest(ctx, "project-1", req.ID); !isStatus(err, http.StatusConflict) {
		t.Fatalf("second dismiss = %v, want 409", err)
	}
}

// A child is held to its parent's routes as one of its lists, so a change to
// a parent marks the discoboxes holding its children too.
func TestChangingAParentReachesItsChildren(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	child := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH", ParentID: parent.ID})
	if _, err := svc.UpdateSecretBinding(ctx, "project-1", parent.ID, services.UpdateSecretBindingBody{
		Routes: []serverapi.SecretRouteInput{{Hosts: []string{"github.com"}, Paths: []string{"/admin"}, Blocked: serverapi.NewOptBool(true)}, {Hosts: []string{"github.com"}}},
	}); err != nil {
		t.Fatal(err)
	}
	var marked []string
	for _, binding := range marks.bindings {
		marked = append(marked, binding.ID)
	}
	if !slices.Contains(marked, child.ID) {
		t.Fatalf("marked = %v, want the child %s's holders marked", marked, child.ID)
	}
}

// A discobox is shown no refresh request, in the listing as in the inbox:
// renewing a credential is a person's to decide.
func TestADiscoboxIsShownNoRefreshRequest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	if err := st.CreateSecretRefreshRequest(ctx, &model.SecretRefreshRequest{ProjectID: "project-1", SecretID: secret.ID, Cause: model.SecretRefreshCauseStale, Status: model.SecretRefreshStatusPending}); err != nil {
		t.Fatal(err)
	}
	if listed, err := svc.ListSecretRefreshRequests(asLead(), "project-1", ""); err != nil || len(listed) != 0 {
		t.Fatalf("listed as a discobox = %+v, %v; want none", listed, err)
	}
	if listed, err := svc.ListSecretRefreshRequests(ctx, "project-1", ""); err != nil || len(listed) != 1 {
		t.Fatalf("listed as a person = %+v, %v; want it", listed, err)
	}
}

// The CLI takes back one half of a binding held for both: `secret unbind
// --delegate` sends delegate=false, which removes everything handed on under
// it, each recorded, and marks their holders; `secret unbind` without it
// sends use=false, which takes the variable away. Neither half left is
// refused, as is delegating from a binding handed on itself, and a use needs
// a free variable.
func TestUpdatingWhatABindingIsFor(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	both := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Use: true, Delegate: true, EnvName: "GH"})
	child := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH", ParentID: both.ID})

	if _, err := svc.UpdateSecretBinding(ctx, "project-1", child.ID, services.UpdateSecretBindingBody{Delegate: serverapi.NewOptBool(true)}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("a child delegating = %v, want 400", err)
	}
	updated, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Delegate: serverapi.NewOptBool(false)})
	if err != nil || updated.Delegate || !updated.Use {
		t.Fatalf("delegate off = %+v, %v; want a use alone", updated, err)
	}
	if _, err := st.SecretBindingByExactID(ctx, "project-1", child.ID); err == nil {
		t.Fatal("the child handed on under the delegation survived it")
	}
	events, err := st.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: child.ID})
	if err != nil || len(events) != 2 || events[0].Event != model.SecretBindingEventDeleted || events[0].Cause != both.ID {
		t.Fatalf("child events = %+v, %v; want its removal caused by the delegation", events, err)
	}
	var marked []string
	for _, binding := range marks.bindings {
		marked = append(marked, binding.ID)
	}
	if !slices.Contains(marked, child.ID) {
		t.Fatalf("marked = %v, want the child's holders", marked)
	}
	if _, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Use: serverapi.NewOptBool(false)}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("leaving neither half = %v, want 400", err)
	}
	delegateOnly, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Use: serverapi.NewOptBool(false), Delegate: serverapi.NewOptBool(true)})
	if err != nil || delegateOnly.Use || delegateOnly.EnvName != "" || !delegateOnly.Delegate {
		t.Fatalf("use off = %+v, %v; want a delegation with no variable", delegateOnly, err)
	}
	if _, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Use: serverapi.NewOptBool(true)}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("use on with no variable = %v, want 400", err)
	}
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Use: true, EnvName: "TAKEN"})
	if _, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Use: serverapi.NewOptBool(true), EnvVar: serverapi.NewOptString("TAKEN")}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("use on in a taken variable = %v, want 409", err)
	}
	again, err := svc.UpdateSecretBinding(ctx, "project-1", both.ID, services.UpdateSecretBindingBody{Use: serverapi.NewOptBool(true), EnvVar: serverapi.NewOptString("FREE")})
	if err != nil || !again.Use || again.EnvName != "FREE" {
		t.Fatalf("use on = %+v, %v", again, err)
	}
	stored, err := st.SecretBindingByExactID(ctx, "project-1", both.ID)
	if err != nil || !stored.Use || !stored.Delegate || stored.EnvName != "FREE" {
		t.Fatalf("stored = %+v, %v; want what the updates wrote", stored, err)
	}
}

// A binding's variable is an environment variable name, wherever it is named.
func TestAVariableIsAnEnvironmentVariableName(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	createBearerSecret(ctx, t, svc)
	for _, env := range []string{"A-B", "1X", "A B", "A=B"} {
		if _, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
			Secret: "github", TargetKind: serverapi.CreateSecretBindingBodyTargetKindProject, EnvVar: serverapi.NewOptString(env),
		}); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("binding in %q = %v, want 400", env, err)
		}
		if _, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString(env), Hosts: []string{"github.com"},
		}); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("ask for %q = %v, want 400", env, err)
		}
	}
}

// A harness config attached to a discobox gives it the config's use
// bindings, never its delegations (ADR 26-10-27-172 §1).
func TestAnAttachedHarnessConfigDelegatesNothing(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	config := &model.HarnessConfig{ID: "hc-attached", ProjectID: "project-1", Slug: "other", Name: "Other"}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: config.ID, Use: true, Delegate: true, EnvName: "GH"})
	if _, err := st.AttachSandboxHarnessConfig(ctx, "project-1", leadID, config.ID); err != nil {
		t.Fatal(err)
	}
	lead, err := st.GetSandbox(ctx, "project-1", leadID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := st.ResolveSandboxBindings(ctx, lead, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effective.UseFor("GH"); !ok || len(effective.Delegate) != 0 {
		t.Fatalf("effective = %+v, want the use and no delegation", effective)
	}
	req := createAgentRequest(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("approve under an attached config's delegation = %v, want 403", err)
	}
	if listed, err := svc.ListSecrets(asLead(), "project-1"); err != nil || len(listed) != 0 {
		t.Fatalf("the lead's delegated secrets = %+v, %v; want none", listed, err)
	}
}

// What applies stops applying at the earlier of the binding's expiry and its
// parent's, and the resolved view says so.
func TestABoundSecretLapsesWithItsParent(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	sooner, later := time.Now().UTC().Add(time.Hour).Truncate(time.Second), time.Now().UTC().Add(2*time.Hour).Truncate(time.Second)
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, ExpiresAt: &sooner})
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH", ParentID: parent.ID, ExpiresAt: &later})
	bound, err := svc.ListBoundSecrets(ctx, "project-1", model.SecretBindingTargetSandbox, testSandboxID)
	if err != nil || len(bound) != 1 || bound[0].ExpiresAt == nil || !bound[0].ExpiresAt.Equal(sooner) {
		t.Fatalf("bound = %+v, %v; want it lapsing with its parent at %v", bound, err, sooner)
	}
}
