package secrets_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/jx"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// askBoth files a request to use the credential in GH_TOKEN and to delegate
// it, each part with its own routes.
func askBoth(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Routes: []string{"GET api.github.com/repos/acme/web/**"}, Delegate: delegateRoutes("github.com/acme/web.git/**"),
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !req.Use.Asked || !req.Delegate.Asked {
		t.Fatalf("request = %+v, want both parts", req)
	}
	return req
}

// One answer holds a decision for each part, and denying one leaves the
// other's approval standing (ADR 26-11-02-022 §4).
func TestOnePartDeniedLeavesTheOtherApproved(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := askBoth(ctx, t, svc)

	answered, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Use: decide(false), Delegate: decide(true),
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if answered.Status != model.SecretRequestStatusApproved || answered.Use.Outcome != model.SecretRequestPartDenied ||
		answered.Use.BindingID != "" || answered.Delegate.Outcome != model.SecretRequestPartApproved || answered.Delegate.BindingID == "" {
		t.Fatalf("request = %+v, want the delegation approved and the use denied", answered)
	}
	bindings, err := st.ListSecretBindings(ctx, "project-1", store.SecretBindingFilter{TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID})
	if err != nil || len(bindings) != 1 || !bindings[0].Delegate || bindings[0].Use ||
		!reflect.DeepEqual(bindings[0].Routes, req.Delegate.Routes) {
		t.Fatalf("bindings = %+v, %v; want the delegation alone, on its own routes", bindings, err)
	}
	got, held, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, held) != "granted" ||
		services.AgentCredentialPartStatus(got.Use, held.Use) != model.SecretRequestStatusDenied ||
		services.AgentCredentialPartStatus(got.Delegate, held.Delegate) != "granted" {
		t.Fatalf("poll = %+v, %+v, %v; want granted with the use denied", got, held, err)
	}

	// Denying every part answers the request denied.
	other, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"}, Delegate: delegateRoutes("github.com"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DenySecretRequest(ctx, "project-1", other.ID); err != nil {
		t.Fatal(err)
	}
	denied, err := svc.GetSecretRequest(ctx, "project-1", other.ID)
	if err != nil || denied.Status != model.SecretRequestStatusDenied || denied.Use.Outcome != model.SecretRequestPartDenied || denied.Delegate.Outcome != model.SecretRequestPartDenied {
		t.Fatalf("denied request = %+v, %v; want both parts denied", denied, err)
	}
}

// An answer names a decision for every part the request has and no other, so
// a delegation is never granted by omission; a refused answer writes nothing.
func TestAnAnswerDecidesEveryPart(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	both := askBoth(ctx, t, svc)
	useOnly := createAgentRequest(ctx, t, svc)
	for name, tc := range map[string]struct {
		req  *model.SecretRequest
		body services.ApproveSecretRequestBody
	}{
		"the delegate part left out":     {both, approveUse(secret.ID)},
		"the use part left out":          {both, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Delegate: decide(true)}},
		"a part the request lacks":       {useOnly, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Use: decide(true), Delegate: decide(true)}},
		"routes on a denied part":        {useOnly, services.ApproveSecretRequestBody{Use: serverapi.NewOptSecretRequestPartDecision(serverapi.SecretRequestPartDecision{Decision: serverapi.SecretRequestPartDecisionDecisionDeny, TtlSeconds: serverapi.NewOptInt64(5)})}},
		"approving with no secret named": {useOnly, services.ApproveSecretRequestBody{Use: decide(true)}},
	} {
		if _, err := svc.ApproveSecretRequest(ctx, "project-1", tc.req.ID, tc.body); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("%s = %v, want 400", name, err)
		}
	}
	for _, id := range []string{both.ID, useOnly.ID} {
		if got, err := st.GetSecretRequest(ctx, "project-1", id); err != nil || got.Status != model.SecretRequestStatusPending {
			t.Fatalf("request %s = %+v, %v; want it still pending", id, got, err)
		}
	}
	if bindings, _ := st.ListSecretBindings(ctx, "project-1", store.SecretBindingFilter{}); len(bindings) != 0 {
		t.Fatalf("bindings = %+v, want none written", bindings)
	}
}

// An approved use part replaces the discobox's use bindings of that secret:
// the one in the asked variable is kept, every other use-only one goes, and
// another secret's binding and the harness config's and project's are not
// touched (ADR 26-11-02-022 §5).
func TestAnApprovedUsePartReplacesTheUseBindingsOfItsSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	other := createRoutedSecret(ctx, t, svc, "other", "*", 0)
	oldest := mustDeliverAs(t, st, testSandboxID, secret.ID, "GH_A")
	asked := mustDeliverAs(t, st, testSandboxID, secret.ID, "GITHUB_TOKEN")
	unrelated := mustDeliverAs(t, st, testSandboxID, other.ID, "OTHER")
	project := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "GH_PROJECT"})

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc).ID, approveUse(secret.ID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Use.BindingID != asked.ID {
		t.Fatalf("binding = %s, want the one in the asked variable (%s) kept", approved.Use.BindingID, asked.ID)
	}
	ids := bindingIDs(t, st)
	if ids[oldest.ID] || !ids[asked.ID] || !ids[unrelated.ID] || !ids[project.ID] {
		t.Fatalf("bindings = %v, want %s gone and the rest kept", ids, oldest.ID)
	}
	kept, _ := st.SecretBindingByExactID(ctx, "project-1", asked.ID)
	if !reflect.DeepEqual(kept.Routes, []model.SecretRoute{{Hosts: []string{"api.github.com"}}}) || kept.ExpiresAt == nil {
		t.Fatalf("kept binding = %+v, want the approval's routes and lifetime", kept)
	}

	// With none in the asked variable, the oldest moves to it.
	svc2, st2 := newAgentCredentialService(t)
	secret2 := createBearerSecret(ctx, t, svc2)
	// Written an hour apart, so which is oldest does not hang on the clock's
	// resolution (on Windows two writes in a row can share a timestamp).
	first := mustBindAs(t, st2, &model.SecretBinding{SecretID: secret2.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_A", CreatedAt: time.Now().Add(-time.Hour)})
	second := mustDeliverAs(t, st2, testSandboxID, secret2.ID, "GH_B")
	approved, err = svc2.ApproveSecretRequest(ctx, "project-1", createAgentRequest(ctx, t, svc2).ID, approveUse(secret2.ID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	moved, err := st2.SecretBindingByExactID(ctx, "project-1", first.ID)
	if err != nil || approved.Use.BindingID != first.ID || moved.EnvName != "GITHUB_TOKEN" || bindingIDs(t, st2)[second.ID] {
		t.Fatalf("approved %s, oldest %+v (%v); want the oldest moved to GITHUB_TOKEN and %s gone", approved.Use.BindingID, moved, err, second.ID)
	}
	events, _ := st2.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: second.ID})
	if len(events) == 0 || events[0].Event != model.SecretBindingEventDeleted || events[0].Cause != approved.ID { // newest first
		t.Fatalf("events of the deleted binding = %+v, want its deletion caused by the request", events)
	}
}

// An approved delegate part updates the delegation in place: it keeps its ID,
// so what was handed on under it stays (ADR 26-11-02-022 §5).
func TestAnApprovedDelegatePartKeepsWhatWasHandedOn(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	delegation := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Delegate: true})
	child := handOnTo(t, st, secret.ID, delegation.ID)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Delegate: delegateRoutes("GET api.github.com/repos/acme/**"),
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Delegate: decide(true)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	updated, _ := st.SecretBindingByExactID(ctx, "project-1", delegation.ID)
	if approved.Delegate.BindingID != delegation.ID || !reflect.DeepEqual(updated.Routes, req.Delegate.Routes) || !bindingIDs(t, st)[child.ID] {
		t.Fatalf("delegation %+v, approved %s; want it updated in place and its child kept", updated, approved.Delegate.BindingID)
	}
}

// A binding that both uses and delegates is split, never deleted: approving
// the delegate part keeps it as the delegation, with what was handed on
// under it, and its use goes to a binding of its own in its variable, with
// the routes it had; approving the use part takes only its use side.
func TestACombinedBindingIsSplitNeverDeleted(t *testing.T) {
	ctx := testPrincipalContext()
	for _, part := range []string{"delegate", "use"} {
		t.Run(part, func(t *testing.T) {
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			oldRoutes := []model.SecretRoute{{Hosts: []string{"uploads.github.com"}}}
			combined := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
				Use: true, Delegate: true, EnvName: "GH_TOKEN", Routes: oldRoutes})
			child := handOnTo(t, st, secret.ID, combined.ID)
			req := askBoth(ctx, t, svc)
			body := services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID), Use: decide(part == "use"), Delegate: decide(part == "delegate")}
			if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body); err != nil {
				t.Fatalf("approve: %v", err)
			}
			standing, err := st.SecretBindingByExactID(ctx, "project-1", combined.ID)
			if err != nil || standing.Use || !standing.Delegate || standing.EnvName != "" || !bindingIDs(t, st)[child.ID] {
				t.Fatalf("standing = %+v, %v; want it the delegation alone, its child kept", standing, err)
			}
			uses, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetSandbox, testSandboxID)
			if err != nil || len(uses) != 1 || uses[0].EnvName != "GH_TOKEN" || uses[0].ID == combined.ID {
				t.Fatalf("use bindings = %+v, %v; want one of its own in GH_TOKEN", uses, err)
			}
			wantUse, wantDelegate := oldRoutes, req.Delegate.Routes
			if part == "use" {
				wantUse, wantDelegate = req.Use.Routes, oldRoutes
			}
			if !reflect.DeepEqual(uses[0].Routes, wantUse) || !reflect.DeepEqual(standing.Routes, wantDelegate) {
				t.Fatalf("use routes %+v, delegate routes %+v; want %+v and %+v", uses[0].Routes, standing.Routes, wantUse, wantDelegate)
			}
		})
	}
}

// Both parts approved over a binding that uses and delegates: the use part
// takes its use away into a binding of its own in the asked variable, and
// the delegate part then updates what is left — the delegation alone — in
// place, so one use binding is made, not two. This is bindingplan.Answer,
// the plan the CLI and the console show before it is approved.
func TestBothPartsOverACombinedBindingMakeOneUseBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	combined := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, Delegate: true, EnvName: "GH_LEAD", Routes: []model.SecretRoute{{Hosts: []string{"uploads.github.com"}}}})
	child := handOnTo(t, st, secret.ID, combined.ID)
	use, delegate := bindingplan.Answer([]bindingplan.Binding{{ID: combined.ID, Use: true, Delegate: true, EnvName: "GH_LEAD"}}, "GH_TOKEN", true, true)
	req := askBoth(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(secret.ID), Use: decide(true), Delegate: decide(true),
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	bindings, err := st.ListSecretBindings(ctx, "project-1", store.SecretBindingFilter{TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %+v, %v; want the delegation and one use binding", bindings, err)
	}
	for _, b := range bindings {
		switch {
		case b.ID == combined.ID:
			if b.Use || !b.Delegate || !reflect.DeepEqual(b.Routes, req.Delegate.Routes) {
				t.Errorf("delegation = %+v, want it alone, on the delegate part's routes", b)
			}
		case !b.Use || b.Delegate || b.EnvName != "GH_TOKEN" || !reflect.DeepEqual(b.Routes, req.Use.Routes):
			t.Errorf("use binding = %+v, want one in GH_TOKEN on the use part's routes", b)
		}
	}
	if !bindingIDs(t, st)[child.ID] {
		t.Error("what was handed on under the delegation went with it")
	}
	if use.Keep != "" || delegate.Keep != combined.ID || delegate.Steps[0].Fate != bindingplan.Updated {
		t.Errorf("plan = %+v / %+v; it does not say what the server did", use, delegate)
	}
}

// A discobox holds at most one sandbox-level delegate binding of a secret:
// the store refuses a second (idx_secret_binding_delegate), which is what
// bindingplan.Delegate relies on.
func TestADiscoboxHoldsOneDelegationOfASecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Delegate: true})
	second := &model.SecretBinding{ProjectID: "project-1", SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, Delegate: true, EnvName: "GH_TOKEN"}
	if err := st.CreateSecretBinding(ctx, second, store.BindingChange{Actor: "user-1"}); err == nil {
		t.Fatal("a second delegate binding of the secret on one discobox was stored")
	}
}

// Every binding a replacement would update, split or delete is checked, and
// any refusal refuses the whole approval with nothing written: one holding
// the value itself is renewed or removed only by a raw grant, a combined one
// holding it is not split, and one under another parent is not taken over.
func TestAReplacementRefusesBindingsItMayNotTouch(t *testing.T) {
	ctx := testPrincipalContext()
	for name, tc := range map[string]struct {
		standing func(st *store.Store, secretID string) *model.SecretBinding
		answer   func(secretID string) services.ApproveSecretRequestBody
	}{
		"a raw binding in another variable": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_RAW", Raw: "env"})
			},
			answer: func(secretID string) services.ApproveSecretRequestBody {
				return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secretID), Use: decide(true), Delegate: decide(false)}
			},
		},
		"a combined raw binding": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Delegate: true, EnvName: "GH_RAW", Raw: "env"})
			},
			answer: func(secretID string) services.ApproveSecretRequestBody {
				return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secretID), Use: decide(false), Delegate: decide(true)}
			},
		},
		"a binding handed on under a delegation": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_CHILD", ParentID: parent.ID})
			},
			answer: func(secretID string) services.ApproveSecretRequestBody {
				return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secretID), Use: decide(true), Delegate: decide(true)}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			allowRaw(ctx, t, svc, secret)
			standing := tc.standing(st, secret.ID)
			before := bindingIDs(t, st)
			req := askBoth(ctx, t, svc)
			_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, tc.answer(secret.ID))
			if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), standing.ID) {
				t.Fatalf("approve = %v, want a 409 naming %s", err, standing.ID)
			}
			if after := bindingIDs(t, st); !reflect.DeepEqual(after, before) {
				t.Fatalf("bindings = %v, want %v untouched", after, before)
			}
			got, _ := st.SecretBindingByExactID(ctx, "project-1", standing.ID)
			if got.Use != standing.Use || got.Delegate != standing.Delegate || got.EnvName != standing.EnvName {
				t.Fatalf("standing = %+v, want it untouched", got)
			}
			if got, _ := st.GetSecretRequest(ctx, "project-1", req.ID); got.Status != model.SecretRequestStatusPending {
				t.Fatalf("request = %+v, want it still pending", got)
			}
		})
	}
}

// A part asked by policy documents resolves them on the server: its routes
// are what they resolve to, and each is recorded with what an approver is
// shown of it (ADR 26-11-02-022 §§2–3). An ask over a limit, or with a
// document that does not resolve, is refused naming it.
func TestAPartAskedByPoliciesResolvesThemOnTheServer(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	const doc = `{"version":1,"description":"Open pull requests in one repository","parameters":{"owner":{"type":"pathSegment"},"repo":{"type":"pathSegment"}},"routes":[{"hosts":["api.github.com"],"methods":["POST"],"paths":["/repos/${owner}/${repo}/pulls"]}]}`
	policy := func(file, document string, args map[string]string) serverapi.SandboxCredentialPolicy {
		return serverapi.SandboxCredentialPolicy{File: file, Document: jx.Raw(document), Args: serverapi.NewOptSandboxCredentialPolicyArgs(args)}
	}
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
		Policies: []serverapi.SandboxCredentialPolicy{policy("roles/github-submit.json", doc, map[string]string{"owner": "acme", "repo": "web"})},
		Delegate: serverapi.NewOptSandboxCredentialRequestDelegate(serverapi.SandboxCredentialRequestDelegate{Policies: []serverapi.SandboxCredentialPolicy{
			policy("roles/github-submit.json", doc, map[string]string{"owner": "acme", "repo": "vm"}),
			policy("roles/github-submit.json", doc, map[string]string{"owner": "acme", "repo": "api"}),
		}}),
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	route := func(repo string) model.SecretRoute {
		return model.SecretRoute{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}, Paths: []string{"/repos/acme/" + repo + "/pulls"}}
	}
	if !reflect.DeepEqual(req.Use.Routes, []model.SecretRoute{route("web")}) || len(req.Delegate.Routes) != 2 {
		t.Fatalf("routes: use %+v, delegate %+v", req.Use.Routes, req.Delegate.Routes)
	}
	got := req.Use.Policies
	if len(got) != 1 || got[0].Name != "github-submit" || got[0].File != "roles/github-submit.json" || got[0].Description != "Open pull requests in one repository" ||
		got[0].Digest == "" || got[0].Digest != req.Delegate.Policies[0].Digest || !reflect.DeepEqual(got[0].Routes, []model.SecretRoute{route("web")}) ||
		!reflect.DeepEqual(got[0].Args, map[string]string{"owner": "acme", "repo": "web"}) {
		t.Fatalf("policies = %+v", got)
	}

	tooMany := make([]serverapi.SandboxCredentialPolicy, agentcreds.MaxRequestPolicies+1)
	for i := range tooMany {
		tooMany[i] = policy(fmt.Sprintf("p%d.json", i), doc, map[string]string{"owner": "acme", "repo": "web"})
	}
	var manyRoutes []string
	for i := range agentcreds.MaxPartRoutes + 1 {
		manyRoutes = append(manyRoutes, fmt.Sprintf(`{"hosts":["h%d.example.com"]}`, i))
	}
	wide := `{"version":1,"routes":[` + strings.Join(manyRoutes, ",") + `]}`
	for name, tc := range map[string]struct {
		body services.CreateSandboxCredentialRequestBody
		want string
	}{
		"policies beside routes": {services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"api.github.com"},
			Policies: []serverapi.SandboxCredentialPolicy{policy("a.json", doc, map[string]string{"owner": "acme", "repo": "web"})}}, "not both"},
		"a document that does not resolve": {services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Policies: []serverapi.SandboxCredentialPolicy{policy("roles/broken.json", doc, map[string]string{"owner": "acme"})}}, "roles/broken.json"},
		"too many policies": {services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Policies: tooMany}, "at most"},
		"too large a document": {services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Policies: []serverapi.SandboxCredentialPolicy{policy("big.json", `{"version":1,"description":"`+strings.Repeat("x", agentcreds.MaxPolicyBytes)+`","routes":[{"hosts":["a.example"]}]}`, nil)}}, "big.json"},
		"too many routes": {services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"),
			Policies: []serverapi.SandboxCredentialPolicy{policy("wide.json", wide, nil)}}, "at most"},
	} {
		tc.body.SandboxId = testSandboxID
		if _, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, tc.body); !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %v, want a 400 naming %q", name, err, tc.want)
		}
	}
}

// A request's limit bounds what it resolves to, not only what it sent: small
// documents that expand their arguments into large routes are refused once
// the request's routes outgrow agentcreds.MaxRequestRouteBytes, and nothing
// is filed (ADR 26-11-02-022 §2).
func TestARequestIsBoundedByWhatItResolvesTo(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	var paths []string
	for i := range 400 {
		paths = append(paths, fmt.Sprintf(`"/${a}/%d"`, i))
	}
	doc := `{"version":1,"parameters":{"a":{"type":"pathSegment"}},"routes":[{"hosts":["api.github.com"],"paths":[` + strings.Join(paths, ",") + `]}]}`
	if len(doc) > agentcreds.MaxPolicyBytes/4 {
		t.Fatalf("the document is %d bytes; it should be small, its routes large", len(doc))
	}
	var policies []serverapi.SandboxCredentialPolicy
	for i := range agentcreds.MaxRequestPolicies {
		policies = append(policies, serverapi.SandboxCredentialPolicy{File: fmt.Sprintf("p%d.json", i), Document: jx.Raw(doc),
			Args: serverapi.NewOptSandboxCredentialPolicyArgs(map[string]string{"a": strings.Repeat("a", 250)})})
	}
	_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Policies: policies,
	})
	if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), fmt.Sprint(agentcreds.MaxRequestRouteBytes)) {
		t.Fatalf("err = %v, want a 400 naming the %d-byte limit", err, agentcreds.MaxRequestRouteBytes)
	}
	requests, err := st.ListSecretRequests(ctx, "project-1", "")
	if err != nil || len(requests) != 0 {
		t.Fatalf("requests = %d, %v; want none filed", len(requests), err)
	}
}

// handOnTo binds a secret for use on a discobox testSandboxID created, under
// parentID, as a lead handing it on does.
func handOnTo(t *testing.T, st *store.Store, secretID, parentID string) *model.SecretBinding {
	t.Helper()
	ctx := context.Background()
	creator := testSandboxID
	worker := &model.Sandbox{ID: "sbx-worker-" + parentID, ProjectID: "project-1", Name: "worker-" + parentID, PoolID: testPoolID, CreatedBySandboxID: &creator, ParentSandboxID: &creator}
	if err := st.CreateSandbox(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: worker.ID, Use: true, EnvName: "GH_TOKEN", ParentID: parentID})
}

// bindingIDs are the project's bindings, by ID.
func bindingIDs(t *testing.T, st *store.Store) map[string]bool {
	t.Helper()
	bindings, err := st.ListSecretBindings(context.Background(), "project-1", store.SecretBindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, binding := range bindings {
		out[binding.ID] = true
	}
	return out
}
