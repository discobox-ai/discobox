package secrets_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

const (
	testPoolID    = "pool-1"
	testSandboxID = "sbx-1"
	// leadID is the discobox that created testSandboxID, which the
	// delegation tests act as.
	leadID = "sbx-lead"
)

// sentinelMarks records what a change marked for delivery.
type sentinelMarks struct {
	secrets  []string
	bindings []model.SecretBinding
}

func (m *sentinelMarks) MarkSecretHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, secretID string) error {
	m.secrets = append(m.secrets, secretID)
	return nil
}

func (m *sentinelMarks) MarkBindingHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _ string, bindings []model.SecretBinding) error {
	m.bindings = append(m.bindings, bindings...)
	return nil
}

func asLead() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: leadID, ProjectID: "project-1", UserID: "user-1"})
}

func TestAgentCredentialRequestIsPendingAndRecordsWhatWasAsked(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)

	req := createAgentRequest(ctx, t, svc)
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending", req.Status)
	}
	if req.Name != "github" || req.EnvName != "GITHUB_TOKEN" || !reflect.DeepEqual(req.Routes, []model.SecretRoute{{Hosts: []string{"api.github.com"}}}) ||
		req.Justification == "" || req.Purpose != model.SecretPurposeUse || req.RequestedBy != "agent:"+testSandboxID {
		t.Fatalf("request = %#v, want the ask recorded verbatim, one route per host", req)
	}
}

// A use is delivered in the variable it names, so one with no valid variable is
// refused; a delegation delivers nothing to the discobox that asks, so one that
// names a variable is a mistake about what it asks for.
func TestAgentCredentialRequestNamesAVariableOnlyForAUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	delegate := serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate)
	for _, tc := range []struct {
		name string
		body services.CreateSandboxCredentialRequestBody
		want int
	}{
		{"a use with no variable", services.CreateSandboxCredentialRequestBody{Name: "github", Hosts: []string{"api.github.com"}}, http.StatusBadRequest},
		{"a use with a malformed variable", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("A=B"), Hosts: []string{"api.github.com"}}, http.StatusBadRequest},
		{"a delegation naming a variable", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"github.com"}, Purpose: delegate}, http.StatusBadRequest},
		{"no host", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN")}, http.StatusBadRequest},
		{"a host that is no host name", services.CreateSandboxCredentialRequestBody{Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.*.com"}}, http.StatusBadRequest},
		{"a delegation", services.CreateSandboxCredentialRequestBody{Name: "github", Hosts: []string{"github.com"}, Purpose: delegate}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body.SandboxId = testSandboxID
			_, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, tc.body)
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("create = %v, want it filed", err)
				}
				return
			}
			if !isStatus(err, tc.want) {
				t.Fatalf("create = %v, want %d", err, tc.want)
			}
		})
	}
}

// An agent that asks again finds its open ask rather than filing another; an
// ask for anything approving would bind differently is its own.
func TestAgentCredentialRequestReusesAnOpenAsk(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	first := createAgentRequest(ctx, t, svc)
	if again := createAgentRequest(ctx, t, svc); again.ID != first.ID {
		t.Fatalf("second ask = %s, want the open %s", again.ID, first.ID)
	}
	other, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
		GrantTTLSeconds: serverapi.NewOptInt64(7200),
	})
	if err != nil || other.ID == first.ID || other.TTL != 7200 {
		t.Fatalf("ask for another lifetime = %+v, %v; want its own request", other, err)
	}
}

// An agent asks with routes beside hosts: each host is one route to that
// exact host, and each route is read in the typed form. An ask that names
// neither, or a route that does not parse, is refused naming it; and an ask
// repeats an open one when it asks for the same routes in any order.
func TestAgentCredentialRequestAsksWithRoutes(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	ask := func(hosts, routes []string) (*model.SecretRequest, error) {
		return svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
			SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: hosts, Routes: routes,
		})
	}
	first, err := ask([]string{"uploads.github.com"}, []string{"GET api.github.com/repos/acme/web/**", "api.github.com/graphql graphql=query"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	want := []model.SecretRoute{
		{Hosts: []string{"uploads.github.com"}},
		{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{"/repos/acme/web/**"}},
		{Hosts: []string{"api.github.com"}, Paths: []string{"/graphql"}, GraphQL: &secretroute.GraphQL{Operations: []string{"query"}}},
	}
	if !reflect.DeepEqual(first.Routes, want) {
		t.Fatalf("routes = %+v, want %+v", first.Routes, want)
	}
	again, err := ask(nil, []string{"api.github.com/graphql graphql=query", "uploads.github.com", "GET api.github.com/repos/acme/web/**"})
	if err != nil || again.ID != first.ID {
		t.Fatalf("the same routes in another order = %+v, %v; want the open %s", again, err, first.ID)
	}
	other, err := ask(nil, []string{"api.github.com/graphql graphql=mutation"})
	if err != nil || other.ID == first.ID {
		t.Fatalf("other routes = %+v, %v; want a request of their own", other, err)
	}

	if _, err := ask(nil, nil); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("an ask naming no destination = %v, want 400", err)
	}
	_, err = ask(nil, []string{"api.github.com/x graphql=delete"})
	if !isStatus(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "api.github.com/x graphql=delete") {
		t.Fatalf("a route that does not parse = %v, want 400 naming it", err)
	}
}

// A person's approval binds the secret on the discobox that asked, in the
// variable it named and to the routes it asked for, records who decided and
// what it made, and marks the discobox to be sent it — all at once.
func TestApprovingAnAgentRequestBindsTheSecretOnTheSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.Name)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != model.SecretRequestStatusApproved || approved.SecretID != secret.ID || approved.DecidedBy != "user-1" || approved.DecidedAt == nil {
		t.Fatalf("request = %+v, want it decided by the person", approved)
	}
	binding, err := st.SecretBindingByExactID(ctx, "project-1", approved.BindingID)
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if binding.TargetKind != model.SecretBindingTargetSandbox || binding.TargetID != testSandboxID || !binding.Use || binding.Delegate ||
		binding.EnvName != "GITHUB_TOKEN" || binding.RequestID != req.ID || binding.CreatedBy != "user-1" ||
		!reflect.DeepEqual(binding.Routes, []model.SecretRoute{{Hosts: []string{"api.github.com"}}}) {
		t.Fatalf("binding = %+v", binding)
	}
	if len(marks.bindings) != 1 || marks.bindings[0].ID != binding.ID {
		t.Fatalf("marks = %+v, want the binding's holders marked", marks.bindings)
	}
	events, err := st.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: binding.ID})
	if err != nil || len(events) != 1 || events[0].Event != model.SecretBindingEventCreated || events[0].Cause != req.ID || events[0].Actor != "user-1" {
		t.Fatalf("events = %+v, %v; want the creation caused by the request", events, err)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("second approval = %v, want 409", err)
	}
}

// An ask to delegate is answered with a delegate binding, which delivers
// nothing to the discobox that holds it.
func TestApprovingAnAskToDelegateMakesADelegateBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Hosts: []string{"github.com"},
		Purpose: serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	binding, err := st.SecretBindingByExactID(ctx, "project-1", approved.BindingID)
	if err != nil || binding.Use || !binding.Delegate || binding.EnvName != "" {
		t.Fatalf("binding = %+v, %v; want a delegate binding and nothing to use", binding, err)
	}
}

// A variable already holding another credential is not taken over by an
// approval; one holding the same secret is extended.
func TestApprovingAVariableHeldByAnotherSecretIsRefused(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	other := createRoutedSecret(ctx, t, svc, "other", "*", 0)
	mustDeliverAs(t, st, testSandboxID, other.ID, "GITHUB_TOKEN")
	req := createAgentRequest(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("approve = %v, want 409", err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: serverapi.NewOptString(other.ID), TtlSeconds: serverapi.NewOptInt64(600),
	})
	if err != nil {
		t.Fatalf("approve with the secret it holds: %v", err)
	}
	bindings, err := st.ListTargetUseBindings(ctx, "project-1", model.SecretBindingTargetSandbox, testSandboxID)
	if err != nil || len(bindings) != 1 || bindings[0].ID != approved.BindingID || bindings[0].ExpiresAt == nil {
		t.Fatalf("bindings = %+v, %v; want the standing one extended", bindings, err)
	}
}

// What a sandbox holds is listed from its effective bindings: each use in its
// variable with the hosts it may go to, each delegation with none.
func TestGrantedCredentialIsListedForItsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createRoutedSecret(ctx, t, svc, "github", "github.com", 0)
	project := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "GH_TOKEN"})
	delegate := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Delegate: true,
		Routes: []model.SecretRoute{{Hosts: []string{"api.github.com"}}}})

	credentials, err := svc.ListSandboxCredentials(ctx, testPoolID, testSandboxID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []services.SandboxCredential{
		{Name: "github", EnvName: "GH_TOKEN", SecretID: secret.ID, BindingID: project.ID, Hosts: []string{"github.com"}, Purpose: model.SecretPurposeUse},
		{Name: "github", SecretID: secret.ID, BindingID: delegate.ID, Hosts: []string{"api.github.com"}, Purpose: model.SecretPurposeDelegate},
	}
	if !reflect.DeepEqual(credentials, want) {
		t.Fatalf("credentials = %+v, want %+v", credentials, want)
	}
}

func TestAgentCredentialCallsRefuseAnotherPoolsSandbox(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	if _, err := svc.ListSandboxCredentials(ctx, "pool-other", testSandboxID); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("list = %v, want 404", err)
	}
	if _, err := svc.CreateSandboxCredentialRequest(ctx, "pool-other", services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
	}); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("ask = %v, want 404", err)
	}
}

// Polling says granted while the binding an approval made stands, and denied
// once it is removed.
func TestPollingReportsGrantedOnceApprovedAndDeniedOnceRemoved(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	req := createAgentRequest(ctx, t, svc)
	got, binding, err := svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, binding) != model.SecretRequestStatusPending {
		t.Fatalf("poll = %+v, %v; want pending", got, err)
	}
	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
	if err != nil {
		t.Fatal(err)
	}
	got, binding, err = svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, binding) != "granted" {
		t.Fatalf("poll after approval = %+v, %v; want granted", got, err)
	}
	if _, err := svc.DeleteSecretBinding(ctx, "project-1", approved.BindingID); err != nil {
		t.Fatal(err)
	}
	got, binding, err = svc.GetSandboxCredentialRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || services.AgentCredentialRequestStatus(got, binding) != model.SecretRequestStatusDenied {
		t.Fatalf("poll after removal = %+v, %v; want denied", got, err)
	}
}

// A discobox answers a request from a discobox it created only under a live
// delegate binding covering it of the secret it answers with: the binding is
// handed on under it, for use alone, and lasts no longer (ADR 26-10-27-880
// §3). Named longer, it is refused.
func TestADiscoboxApprovesUnderItsDelegateBinding(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	parentExpiry := time.Now().UTC().Add(30 * time.Minute)
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true, ExpiresAt: &parentExpiry})
	req := createAgentRequest(ctx, t, svc)

	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{TtlSeconds: serverapi.NewOptInt64(7200)}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("approve for longer than the delegation = %v, want 403", err)
	}
	approved, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, services.ApproveSecretRequestBody{})
	if err != nil {
		t.Fatalf("approve as the lead: %v", err)
	}
	child, err := st.SecretBindingByExactID(ctx, "project-1", approved.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != parent.ID || !child.Use || child.Delegate || child.CreatedBy != leadID || approved.DecidedBy != leadID ||
		child.ExpiresAt == nil || child.ExpiresAt.After(parentExpiry) {
		t.Fatalf("child = %+v, want a use handed on under %s ending by %v", child, parent.ID, parentExpiry)
	}
}

// What only a person may do: answer an ask to delegate, and hand on what no
// delegate binding covers.
func TestADiscoboxHandsOnOnlyWhatItWasDelegated(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	secret := createBearerSecret(ctx, t, svc)
	use := createAgentRequest(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", use.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("approve with no delegate binding = %v, want 403", err)
	}
	mustBindAs(t, st, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Delegate: true})
	ask, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", Hosts: []string{"github.com"},
		Purpose: serverapi.NewOptCreateSandboxCredentialRequestBodyPurpose(serverapi.CreateSandboxCredentialRequestBodyPurposeDelegate),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", ask.ID, services.ApproveSecretRequestBody{}); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("approve an ask to delegate as a discobox = %v, want 403", err)
	}
	// A delegation on the project covers the lead too.
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", use.ID, services.ApproveSecretRequestBody{}); err != nil {
		t.Fatalf("approve under the project's delegation: %v", err)
	}
	listed, err := svc.ListSecrets(asLead(), "project-1")
	if err != nil || len(listed) != 1 || listed[0].ID != secret.ID {
		t.Fatalf("the lead's secrets = %+v, %v; want only the one it was delegated", listed, err)
	}
}

// A discobox listing requests sees only those filed by discoboxes it created;
// a person sees every one (ADR 26-09-30-782 §2).
func TestADiscoboxListsOnlyItsOwnDiscoboxesRequests(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-other", ProjectID: "project-1", Name: "other", PoolID: testPoolID}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	for id, sandbox := range map[string]string{"sreq-worker": testSandboxID, "sreq-other": "sbx-other"} {
		if err := st.CreateSecretRequest(ctx, &model.SecretRequest{
			ID: id, ProjectID: "project-1", SandboxID: sandbox, RequestedBy: "agent:" + sandbox, Status: model.SecretRequestStatusPending,
		}); err != nil {
			t.Fatalf("create request %s: %v", id, err)
		}
	}
	owned, err := svc.ListSecretRequests(asLead(), "project-1", "")
	if err != nil || len(owned) != 1 || owned[0].ID != "sreq-worker" {
		t.Fatalf("listed as the lead = %+v, %v; want only its worker's request", owned, err)
	}
	all, err := svc.ListSecretRequests(ctx, "project-1", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("listed as a person = %d, %v; want both", len(all), err)
	}
}

// An approval that names no lifetime binds for what the agent asked, else an
// hour — what the window opens on — and never past the secret's limit.
func TestAnApprovalNamingNoLifetimeBindsForWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		name         string
		asked, limit int64
		want         time.Duration
	}{
		{"what was asked", 7200, 86400, 2 * time.Hour},
		{"an hour when nothing was asked", 0, 86400, time.Hour},
		{"within the secret's limit", 7200, 600, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			secret := createRoutedSecret(ctx, t, svc, "github", "*", tc.limit)
			req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
				SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GITHUB_TOKEN"), Hosts: []string{"api.github.com"},
				GrantTTLSeconds: serverapi.NewOptInt64(tc.asked),
			})
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secret.ID)})
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			binding, err := st.SecretBindingByExactID(ctx, "project-1", approved.BindingID)
			if err != nil || binding.ExpiresAt == nil {
				t.Fatalf("binding = %+v, %v; want one that expires", binding, err)
			}
			if got := time.Until(*binding.ExpiresAt); got > tc.want || got < tc.want-time.Minute {
				t.Fatalf("expires in %v, want %v", got, tc.want)
			}
		})
	}
}

func createAgentRequest(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId:     testSandboxID,
		Name:          "github",
		EnvVar:        serverapi.NewOptString("GITHUB_TOKEN"),
		Hosts:         []string{"api.github.com"},
		Justification: serverapi.NewOptString("the task asks me to open a PR"),
	})
	if err != nil {
		t.Fatalf("create credential request: %v", err)
	}
	return req
}

func createBearerSecret(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.Secret {
	t.Helper()
	return createRoutedSecret(ctx, t, svc, "github", "github.com", 0)
}

// createRoutedSecret creates a token sent to host, with a binding limit when
// limit is positive and none otherwise.
func createRoutedSecret(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, name, host string, limit int64) *model.Secret {
	t.Helper()
	secret, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:                 name,
		Type:                 serverapi.CreateSecretBodyTypeToken,
		Value:                serverapi.SecretValue{Token: serverapi.NewOptString("ghp_realrealrealrealrealrealrealreal12")},
		Routes:               hostRoutes(host),
		MaxBindingTTLSeconds: serverapi.NewOptInt64(limit),
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return secret
}

// mustBindAs stores a binding in project-1 as a person made it.
func mustBindAs(t *testing.T, st *store.Store, binding *model.SecretBinding) *model.SecretBinding {
	t.Helper()
	binding.ProjectID = "project-1"
	if err := st.CreateSecretBinding(context.Background(), binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return binding
}

// mustDeliverAs binds a secret for use in env on a sandbox.
func mustDeliverAs(t *testing.T, st *store.Store, sandboxID, secretID, env string) *model.SecretBinding {
	t.Helper()
	return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sandboxID, Use: true, EnvName: env})
}

// newAgentCredentialService builds a service over a database holding one
// project, one pool, the sandbox that asks, and the lead the delegation tests
// act as, which created it.
func newAgentCredentialService(t *testing.T) (*resourcesecrets.Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	write := db.Write.WithContext(ctx)
	if err := write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := write.Create(&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := write.Create(&model.Pool{
		ID:           testPoolID,
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"},
	}).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := write.Create(&model.Sandbox{ID: leadID, ProjectID: "project-1", Name: "lead", PoolID: testPoolID}).Error; err != nil {
		t.Fatalf("create lead sandbox: %v", err)
	}
	lead := leadID
	if err := write.Create(&model.Sandbox{ID: testSandboxID, ProjectID: "project-1", Name: "sandbox", PoolID: testPoolID, CreatedBySandboxID: &lead}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return resourcesecrets.NewService(st), st
}
