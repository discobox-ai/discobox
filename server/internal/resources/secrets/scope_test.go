package secrets_test

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestApprovalScopesKeepLiveCeilingsAndHistoricalConsent(t *testing.T) {
	for _, scope := range []string{"", "routes", "hosts", "anywhere"} {
		t.Run(scope, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			secret.Routes = []model.SecretRoute{{Hosts: []string{"api.github.com", "github.com"}}, {Hosts: []string{"api.github.com"}, Paths: []string{"/blocked"}, Blocked: true}}
			if err := st.UpdateSecret(ctx, secret); err != nil {
				t.Fatal(err)
			}
			req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{SandboxId: testSandboxID, Name: "github", EnvVar: api.NewOptString("GH_TOKEN"), Routes: []string{"GET api.github.com/one", "GET api.github.com/two"}})
			if err != nil {
				t.Fatal(err)
			}
			body := approveUse(secret.ID)
			part := body.Use.Value
			part.TtlSeconds = api.NewOptInt64(3600)
			if scope != "" {
				part.Scope = api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScope(scope))
			}
			body.Use = api.NewOptSecretRequestPartDecision(part)
			answered, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := st.GetSecretBinding(ctx, "project-1", answered.Use.BindingID)
			if err != nil {
				t.Fatal(err)
			}
			check := func(host, path, method string, want bool) {
				t.Helper()
				got, reason := secretroute.Decide(store.RouteLists(secret, binding, nil), secretroute.Request{Host: host, URL: "https://" + host + path, Method: method})
				if got != want {
					t.Fatalf("%s %s %s = %v (%s), want %v", method, host, path, got, reason, want)
				}
			}
			check("api.github.com", "/one", "GET", true)
			check("api.github.com", "/three", "POST", scope == "hosts" || scope == "anywhere")
			check("github.com", "/other", "GET", scope == "anywhere")
			check("api.github.com", "/blocked", "GET", false)
			check("other.example", "/one", "GET", false)
			stored, err := st.GetSecretRequest(ctx, "project-1", req.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantScope := scope
			if wantScope == "" {
				wantScope = "routes"
			}
			grant := stored.Use.Grant
			if stored.Use.ApprovedScope != wantScope || grant == nil || grant.SecretID != secret.ID || grant.SecretRevision != secret.Revision || grant.ExpiresAt == nil || grant.Delivery != "sentinel" || len(grant.RouteLists) != 2 || !reflect.DeepEqual(stored.Use.Routes, req.Use.Routes) {
				t.Fatalf("consent=%+v, grant=%+v", stored.Use, grant)
			}
			old := *grant
			// Later edits narrow the actual grant without rewriting historical consent.
			secret.Routes = []model.SecretRoute{{Hosts: []string{"api.github.com"}, Paths: []string{"/one"}}}
			if err := st.UpdateSecret(ctx, secret); err != nil {
				t.Fatal(err)
			}
			check("api.github.com", "/three", "POST", false)
			if _, err := st.DeleteSecretBinding(ctx, "project-1", binding.ID, store.BindingChange{Actor: "user-1"}); err != nil {
				t.Fatal(err)
			}
			stored, err = st.GetSecretRequest(ctx, "project-1", req.ID)
			if err != nil || !reflect.DeepEqual(stored.Use.Grant, &old) {
				t.Fatalf("history changed after delete: %+v, %v", stored.Use.Grant, err)
			}
			// Each fresh decision still defaults to requested routes.
			next, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{SandboxId: testSandboxID, Name: "github", EnvVar: api.NewOptString("GH_TOKEN"), Routes: []string{"GET api.github.com/one"}})
			if err != nil {
				t.Fatal(err)
			}
			next, err = svc.ApproveSecretRequest(ctx, "project-1", next.ID, approveUse(secret.ID))
			if err != nil || next.Use.ApprovedScope != "routes" {
				t.Fatalf("next=%+v, %v", next, err)
			}
		})
	}
}

func TestApprovalScopeValidationAndIndependentParts(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := askBoth(ctx, t, svc)
	for _, tc := range []struct {
		name, scope    string
		deny, override bool
	}{
		{"unknown", "bogus", false, false}, {"denied", "anywhere", true, false}, {"hosts with edits", "hosts", false, true}, {"anywhere with edits", "anywhere", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part := api.SecretRequestPartDecision{Decision: api.SecretRequestPartDecisionDecisionApprove, Scope: api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScope(tc.scope))}
			if tc.deny {
				part.Decision = api.SecretRequestPartDecisionDecisionDeny
			}
			if tc.override {
				part.Routes = []api.SecretRouteInput{{Hosts: []string{"github.com"}}}
			}
			_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: api.NewOptString(secret.ID), Use: api.NewOptSecretRequestPartDecision(part), Delegate: decide(false)})
			if !isStatus(err, http.StatusBadRequest) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	p := api.SecretRequestPartDecision{Decision: api.SecretRequestPartDecisionDecisionApprove, Scope: api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScopeAnywhere)}
	got, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: api.NewOptString(secret.ID), Use: api.NewOptSecretRequestPartDecision(p), Delegate: decide(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Use.ApprovedScope != "anywhere" || got.Delegate.ApprovedScope != "routes" {
		t.Fatalf("got=%+v", got)
	}
	if binding, err := st.GetSecretBinding(ctx, "project-1", got.Delegate.BindingID); err != nil || !reflect.DeepEqual(binding.Routes, req.Delegate.Routes) {
		t.Fatalf("delegate=%+v,%v", binding, err)
	}
}

func TestWiderScopeStaysUnderParentDelegation(t *testing.T) {
	for _, scope := range []string{"routes", "hosts", "anywhere"} {
		t.Run(scope, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createBearerSecret(ctx, t, svc)
			parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, Routes: []model.SecretRoute{{Hosts: []string{"github.com"}, Methods: []string{"GET"}}, {Hosts: []string{"github.com"}, Paths: []string{"/private"}, Blocked: true}}})
			end := time.Now().Add(time.Hour)
			parent.ExpiresAt = &end
			if err := st.UpdateSecretBinding(ctx, parent, store.BindingChange{Actor: "user-1"}); err != nil {
				t.Fatal(err)
			}
			req := createAgentRequest(ctx, t, svc)
			part := api.SecretRequestPartDecision{Decision: api.SecretRequestPartDecisionDecisionApprove, Scope: api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScope(scope))}
			got, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: api.NewOptString(secret.ID), Use: api.NewOptSecretRequestPartDecision(part)})
			if err != nil {
				t.Fatal(err)
			}
			b, err := st.GetSecretBinding(ctx, "project-1", got.Use.BindingID)
			if err != nil {
				t.Fatal(err)
			}
			if b.ParentID != parent.ID || b.ExpiresAt == nil || b.ExpiresAt.After(end) || got.Use.Grant.ParentBindingID != parent.ID || len(got.Use.Grant.RouteLists) != 3 {
				t.Fatalf("binding=%+v, grant=%+v", b, got.Use.Grant)
			}
			for _, r := range []secretroute.Request{{Host: "github.com", URL: "https://github.com/private", Method: "GET"}, {Host: "github.com", URL: "https://github.com/other", Method: "POST"}} {
				if ok, _ := secretroute.Decide(store.RouteLists(secret, b, parent), r); ok {
					t.Fatalf("parent ceiling bypassed: %+v", r)
				}
			}
		})
	}
}

func TestRequestedHostsPreservePatternsAndExcludeBlockedOnlyHosts(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	for _, tc := range []struct {
		routes []string
		want   []string
	}{
		{[]string{"GET api.github.com/one", "POST *.example.com/mcp mcp=read*", "denied.example.com=blocked"}, []string{"api.github.com", "*.example.com"}},
		{[]string{"denied.example.com=blocked"}, nil},
	} {
		req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{SandboxId: testSandboxID, Name: "github", EnvVar: api.NewOptString("GH_TOKEN"), Routes: tc.routes})
		if err != nil {
			t.Fatal(err)
		}
		body := approveUse(secret.ID)
		part := body.Use.Value
		part.Scope = api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScopeHosts)
		body.Use = api.NewOptSecretRequestPartDecision(part)
		got, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body)
		if tc.want == nil {
			if !isStatus(err, http.StatusBadRequest) {
				t.Fatalf("blocked-only hosts=%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := st.GetSecretBinding(ctx, "project-1", got.Use.BindingID)
		if err != nil || !reflect.DeepEqual(b.Routes, []model.SecretRoute{{Hosts: tc.want}}) {
			t.Fatalf("host routes=%+v, %v", b, err)
		}
	}
}

func TestAnywhereAdditionAndProtocolCeilings(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	secret.Routes = []model.SecretRoute{{Hosts: []string{"github.com"}, Methods: []string{"POST"}, Paths: []string{"/graphql"}, GraphQL: &secretroute.GraphQL{Operations: []string{"query"}}}}
	if err := st.UpdateSecret(ctx, secret); err != nil {
		t.Fatal(err)
	}
	req := createAgentRequest(ctx, t, svc)
	body := approveUse(secret.ID)
	part := body.Use.Value
	part.Scope = api.NewOptSecretRequestPartDecisionScope(api.SecretRequestPartDecisionScopeAnywhere)
	part.As = api.NewOptSecretRequestPartDecisionAs(api.SecretRequestPartDecisionAsAdd)
	part.TtlSeconds = api.NewOptInt64(3600)
	body.Use = api.NewOptSecretRequestPartDecision(part)
	got, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.GetSecretBinding(ctx, "project-1", got.Use.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Adds || got.Use.Grant.Mode != "add" || got.Use.Grant.ExpiresAt == nil {
		t.Fatalf("grant=%+v", got.Use.Grant)
	}
	for _, tc := range []struct {
		body string
		want bool
	}{{`{"query":"query { viewer { login } }"}`, true}, {`{"query":"mutation { deleteRepository(input: {}) { clientMutationId } }"}`, false}} {
		ok, why := secretroute.Decide(store.RouteLists(secret, b, nil), secretroute.Request{Host: "github.com", URL: "https://github.com/graphql", Method: "POST", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: func() ([]byte, bool) { return []byte(tc.body), true }})
		if ok != tc.want {
			t.Fatalf("protocol ceiling=%v (%s), want %v", ok, why, tc.want)
		}
	}
}
