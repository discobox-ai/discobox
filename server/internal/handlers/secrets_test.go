package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

func TestSecretHandlersDoNotReturnSecretValues(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	listRes, err := h.ListSecrets(context.Background(), serverapi.ListSecretsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	assertResponseDoesNotContain(t, listRes, "clear-token")
	assertResponseDoesNotContain(t, listRes, "encrypted-token")
	assertResponseDoesNotContain(t, listRes, "value")

	getRes, err := h.GetSecret(context.Background(), serverapi.GetSecretParams{ProjectId: "project-1", SecretId: "secret-1"})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	assertResponseDoesNotContain(t, getRes, "clear-token")
	assertResponseDoesNotContain(t, getRes, "encrypted-token")
	assertResponseDoesNotContain(t, getRes, "value")
}

func TestSecretRequestHandlersDoNotReturnSecretValues(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	getRes, err := h.GetSecretRequest(context.Background(), serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get secret request: %v", err)
	}
	assertResponseDoesNotContain(t, getRes, "clear-token")
	assertResponseDoesNotContain(t, getRes, "value")
}

type fakeSecretService struct{}

func (fakeSecretService) ListSecrets(context.Context, string) ([]model.Secret, error) {
	return []model.Secret{fakeSecret()}, nil
}

func (fakeSecretService) CreateSecret(context.Context, string, svcapi.CreateSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) GetSecret(context.Context, string, string) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) UpdateSecret(context.Context, string, string, svcapi.UpdateSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) RefreshSecret(context.Context, string, string, svcapi.RefreshSecretBody) (*model.Secret, error) {
	secret := fakeSecret()
	return &secret, nil
}

func (fakeSecretService) ListSecretRefreshEvents(context.Context, string, svcapi.SecretRefreshFilter) ([]model.SecretRefreshEvent, error) {
	return nil, nil
}

func (fakeSecretService) DeleteSecret(context.Context, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretRequests(context.Context, string, string) ([]model.SecretRequest, error) {
	request := fakeSecretRequest()
	return []model.SecretRequest{request}, nil
}

func (fakeSecretService) GetSecretRequest(context.Context, string, string) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
}

func (fakeSecretService) ApproveSecretRequest(context.Context, string, string, svcapi.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
}

func (fakeSecretService) DenySecretRequest(context.Context, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretRefreshRequests(context.Context, string, string) ([]model.SecretRefreshRequest, error) {
	return nil, nil
}

func (fakeSecretService) DismissSecretRefreshRequest(context.Context, string, string) (*model.SecretRefreshRequest, error) {
	return &model.SecretRefreshRequest{ID: "sref-1", Status: model.SecretRefreshStatusDismissed, Cause: model.SecretRefreshCauseStale}, nil
}

func (fakeSecretService) ListSecretBindings(context.Context, string, svcapi.SecretBindingQuery) ([]model.SecretBinding, error) {
	return []model.SecretBinding{fakeSecretBinding()}, nil
}

func (fakeSecretService) CreateSecretBinding(context.Context, string, svcapi.CreateSecretBindingBody) (*model.SecretBinding, error) {
	binding := fakeSecretBinding()
	return &binding, nil
}

func (fakeSecretService) GetSecretBinding(context.Context, string, string) (*model.SecretBinding, error) {
	binding := fakeSecretBinding()
	return &binding, nil
}

func (fakeSecretService) UpdateSecretBinding(context.Context, string, string, svcapi.UpdateSecretBindingBody) (*model.SecretBinding, error) {
	binding := fakeSecretBinding()
	return &binding, nil
}

func (fakeSecretService) DeleteSecretBinding(context.Context, string, string) ([]string, error) {
	return []string{"bind-1", "bind-2"}, nil
}

func (fakeSecretService) ListSecretBindingEvents(context.Context, string, svcapi.SecretBindingEventQuery) ([]model.SecretBindingEvent, error) {
	binding := fakeSecretBinding()
	return []model.SecretBindingEvent{{ID: "bev-1", ProjectID: "project-1", BindingID: binding.ID, SecretID: binding.SecretID,
		Event: model.SecretBindingEventCreated, Actor: "user-1", Binding: binding, At: binding.CreatedAt}}, nil
}

func (fakeSecretService) ListBoundSecrets(context.Context, string, string, string) ([]svcapi.BoundSecret, error) {
	binding := fakeSecretBinding()
	return []svcapi.BoundSecret{{Binding: binding, SecretName: "github", RouteLists: [][]model.SecretRoute{{{Hosts: []string{"*"}}}, binding.Routes}}}, nil
}

func (fakeSecretService) ResolveSandboxSecret(context.Context, string, string, string, string) (*model.SandboxSecretResolution, error) {
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) RecordSandboxSecretRejection(context.Context, string, string, string, string, string) error {
	return nil
}

func (fakeSecretService) ListSecretRejections(context.Context, string) ([]model.SecretRejection, error) {
	return nil, nil
}

func (fakeSecretService) ListSandboxCredentials(context.Context, string, string) ([]svcapi.SandboxCredential, error) {
	return nil, nil
}

func (fakeSecretService) CreateSandboxCredentialRequest(context.Context, string, svcapi.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) GetSandboxCredentialRequest(context.Context, string, string, string) (*model.SecretRequest, *model.SecretBinding, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil, nil
}

func fakeSecret() model.Secret {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.Secret{
		ID:             "secret-1",
		ProjectID:      "project-1",
		Name:           "github",
		Type:           model.SecretTypeToken,
		Routes:         []model.SecretRoute{{Hosts: []string{"github.com"}}},
		MaxBindingTTL:  3600,
		EncryptedValue: []byte(`{"token":"encrypted-token"}`),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func fakeSecretRequest() model.SecretRequest {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretRequest{
		ID:          "request-1",
		ProjectID:   "project-1",
		RequestedBy: "user-1",
		Routes:      []model.SecretRoute{{Hosts: []string{"api.github.com"}}, {Hosts: []string{"api.githubcopilot.com"}}},
		SecretID:    "secret-1",
		Status:      model.SecretRequestStatusApproved,
		BindingID:   "bind-1",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func fakeSecretBinding() model.SecretBinding {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretBinding{
		ID:         "bind-1",
		ProjectID:  "project-1",
		SecretID:   "secret-1",
		SecretName: "github",
		TargetKind: model.SecretBindingTargetSandbox,
		TargetID:   "sb-1",
		Use:        true,
		EnvName:    "GH_TOKEN",
		Routes:     []model.SecretRoute{{Hosts: []string{"api.github.com"}}, {Hosts: []string{"api.githubcopilot.com"}}},
		CreatedBy:  "user-1",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func assertResponseDoesNotContain(t *testing.T, res any, needle string) {
	t.Helper()
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(data), needle) {
		t.Fatalf("response = %s, did not expect %q", data, needle)
	}
}

// capturingVerdictService records the filter the handler built, so the test
// asserts what reached the service rather than what the handler meant.
// resolvingSecretService approves every resolve with one secret's value.
type resolvingSecretService struct{ fakeSecretService }

func (resolvingSecretService) ResolveSandboxSecret(context.Context, string, string, string, string) (*model.SandboxSecretResolution, error) {
	return &model.SandboxSecretResolution{
		Status:      model.SecretRequestStatusApproved,
		Value:       &model.SecretValue{Token: "real-token"},
		SecretID:    "sec_1",
		ReportScope: []model.HarnessSecretRejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}},
	}, nil
}

// An approved answer names the secret it is, so the pool's proxy can record
// which secret a request spent (ADR 26-10-01-240).
func TestResolveSandboxSecretNamesTheSecret(t *testing.T) {
	h := New(svcapi.Services{Secrets: resolvingSecretService{}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeSecretResolve},
	})
	res, err := h.ResolveSandboxSecret(ctx, &serverapi.ResolveSandboxSecretBody{SandboxId: "sb-1", Sentinel: "SENT", Host: "api.example.com"}, serverapi.ResolveSandboxSecretParams{PoolId: "pool-1"})
	if err != nil {
		t.Fatalf("ResolveSandboxSecret() error = %v", err)
	}
	body, ok := res.(*serverapi.ResolveSandboxSecretResponse)
	if !ok {
		t.Fatalf("response = %T, want the resolution", res)
	}
	if body.Status != serverapi.ResolveSandboxSecretResponseStatusApproved || body.SecretId.Or("") != "sec_1" {
		t.Fatalf("resolution = %+v, want approved and naming sec_1", body)
	}
	// And the requests that answer for it, which the proxy holds its reports
	// to (ADR 26-10-13-595).
	if len(body.ReportScope) != 1 || body.ReportScope[0].Host != "api.example.com" || !slices.Equal(body.ReportScope[0].Paths, []string{"/v1/messages"}) {
		t.Fatalf("report scope = %+v, want api.example.com /v1/messages", body.ReportScope)
	}
}

// A binding and a request carry their routes, and the binding's handlers
// answer in the API's shapes: the resolved view's route lists, a delete's
// cascade, the history's snapshots.
func TestSecretBindingsAndRequestsCarryTheirRoutes(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})
	ctx := context.Background()
	want := []string{"api.github.com", "api.githubcopilot.com"}
	hosts := func(routes []serverapi.SecretRoute) []string {
		var out []string
		for _, route := range routes {
			out = append(out, route.Hosts...)
		}
		return out
	}

	listRes, err := h.ListSecretBindings(ctx, serverapi.ListSecretBindingsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	bindings := listRes.(*serverapi.ListSecretBindingsBody).SecretBindings
	if len(bindings) != 1 || !slices.Equal(hosts(bindings[0].Routes), want) || bindings[0].EnvVar.Or("") != "GH_TOKEN" ||
		bindings[0].TargetKind != serverapi.SecretBindingTargetKindSandbox || bindings[0].SecretName.Or("") != "github" {
		t.Fatalf("bindings = %+v, want routes %q", bindings, want)
	}
	getRes, err := h.GetSecretRequest(ctx, serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if request := getRes.(*serverapi.SecretRequest); !slices.Equal(hosts(request.Routes), want) || request.BindingId.Or("") != "bind-1" {
		t.Fatalf("request = %+v, want routes %q", request, want)
	}
	boundRes, err := h.ListBoundSecrets(ctx, serverapi.ListBoundSecretsParams{ProjectId: "project-1", TargetKind: serverapi.ListBoundSecretsTargetKindSandbox, Target: serverapi.NewOptString("sb-1")})
	if err != nil {
		t.Fatal(err)
	}
	bound := boundRes.(*serverapi.ListBoundSecretsBody).BoundSecrets
	if len(bound) != 1 || len(bound[0].Routes) != 2 || !slices.Equal(bound[0].Routes[0][0].Hosts, []string{"*"}) || bound[0].From != serverapi.BoundSecretFromSandbox {
		t.Fatalf("bound = %+v, want the secret's list then the binding's", bound)
	}
	deleteRes, err := h.DeleteSecretBinding(ctx, serverapi.DeleteSecretBindingParams{ProjectId: "project-1", BindingId: "bind-1"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted := deleteRes.(*serverapi.DeleteSecretBindingResponse).Deleted; !slices.Equal(deleted, []string{"bind-1", "bind-2"}) {
		t.Fatalf("deleted = %v", deleted)
	}
	eventsRes, err := h.ListSecretBindingEvents(ctx, serverapi.ListSecretBindingEventsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatal(err)
	}
	events := eventsRes.(*serverapi.ListSecretBindingEventsBody).SecretBindingEvents
	if len(events) != 1 || events[0].Event != serverapi.SecretBindingEventEventCreated || events[0].Binding.ID != "bind-1" {
		t.Fatalf("events = %+v", events)
	}
	dismissRes, err := h.DismissSecretRefreshRequest(ctx, serverapi.DismissSecretRefreshRequestParams{ProjectId: "project-1", RequestId: "sref-1"})
	if err != nil {
		t.Fatal(err)
	}
	if refresh := dismissRes.(*serverapi.SecretRefreshRequest); refresh.Status != serverapi.SecretRefreshRequestStatusDismissed {
		t.Fatalf("refresh = %+v", refresh)
	}
}

// The approver picks a binding's routes and lifetime, so the status the
// asking sandbox polls says what was picked: the hosts, the variable its
// sentinel is in, and an expiry, or none for a binding that never lapses. A
// delegation is in no variable.
func TestSandboxCredentialRequestStatusCarriesWhatWasGranted(t *testing.T) {
	approved := &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusApproved, Name: "asked", EnvName: "GH_TOKEN"}
	expires := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	timed := agentCredentialRequestStatus(approved, &model.SecretBinding{Use: true, Routes: []model.SecretRoute{{Hosts: []string{"api.github.com"}}}, ExpiresAt: &expires}, nil, "github")
	if got, ok := timed.ExpiresAt.Get(); !ok || !got.Equal(expires) {
		t.Fatalf("expiresAt = %v (set %v), want %v", got, ok, expires)
	}
	if timed.Name.Or("") != "github" || timed.EnvVar.Or("") != "GH_TOKEN" || !slices.Equal(timed.Hosts, []string{"api.github.com"}) ||
		timed.Status != serverapi.SandboxCredentialRequestStatusStatusGranted || timed.Purpose != serverapi.SandboxCredentialRequestStatusPurposeUse {
		t.Fatalf("status = %+v, want the grant's name, variable and hosts", timed)
	}

	forever := agentCredentialRequestStatus(approved, &model.SecretBinding{Use: true}, &model.Secret{Routes: []model.SecretRoute{{Hosts: []string{"github.com"}}}}, "github")
	if !slices.Equal(forever.Hosts, []string{"github.com"}) {
		t.Fatalf("hosts = %v, want the secret's when the binding names none", forever.Hosts)
	}
	if forever.ExpiresAt.IsSet() {
		t.Fatalf("expiresAt = %v on a grant that never expires, want it absent", forever.ExpiresAt)
	}

	delegated := &model.SecretRequest{ID: "sreq-2", Status: model.SecretRequestStatusApproved, Name: "github", EnvName: "X", Purpose: model.SecretPurposeDelegate}
	status := agentCredentialRequestStatus(delegated, &model.SecretBinding{Delegate: true, Routes: []model.SecretRoute{{Hosts: []string{"github.com"}}}}, nil, "github")
	if status.EnvVar.IsSet() || status.Purpose != serverapi.SandboxCredentialRequestStatusPurposeDelegate {
		t.Fatalf("status = %+v, want a delegation in no variable", status)
	}

	pending := agentCredentialRequestStatus(&model.SecretRequest{ID: "sreq-3", Status: model.SecretRequestStatusPending, EnvName: "X"}, nil, nil, "")
	if pending.Name.IsSet() || pending.EnvVar.IsSet() || len(pending.Hosts) != 0 {
		t.Fatalf("pending status = %+v, want nothing granted", pending)
	}
}

// What a sandbox holds is listed with its purpose, a delegation in no
// variable.
func TestListSandboxCredentialsCarriesThePurpose(t *testing.T) {
	h := New(svcapi.Services{Secrets: listingSecretService{}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeCredentialBroker},
	})
	res, err := h.ListSandboxCredentials(ctx, serverapi.ListSandboxCredentialsParams{PoolId: "pool-1", SandboxId: "sb-1"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := res.(*serverapi.ListSandboxCredentialsResponse)
	if !ok || len(body.Credentials) != 2 {
		t.Fatalf("response = %+v, want two credentials", res)
	}
	use, delegate := body.Credentials[0], body.Credentials[1]
	if use.Purpose.Or("") != serverapi.SandboxCredentialPurposeUse || use.EnvVar != "GH_TOKEN" {
		t.Fatalf("use = %+v", use)
	}
	if delegate.Purpose.Or("") != serverapi.SandboxCredentialPurposeDelegate || delegate.EnvVar != "" {
		t.Fatalf("delegate = %+v", delegate)
	}
}

type listingSecretService struct{ fakeSecretService }

func (listingSecretService) ListSandboxCredentials(context.Context, string, string) ([]svcapi.SandboxCredential, error) {
	return []svcapi.SandboxCredential{
		{Name: "github", EnvName: "GH_TOKEN", SecretID: "sec-1", BindingID: "b-1", Hosts: []string{"api.github.com"}, Purpose: model.SecretPurposeUse},
		{Name: "github", SecretID: "sec-1", BindingID: "b-2", Hosts: []string{"github.com"}, Purpose: model.SecretPurposeDelegate},
	}, nil
}

// A harness config whose save-back routes include a blocked one serializes in
// the API's shape (ADR 26-10-27-880 §4).
func TestAHarnessConfigWithABlockedCaptureRouteSerializes(t *testing.T) {
	config := model.HarnessConfig{ID: "hc-1", ProjectID: "project-1", Slug: "codex", Name: "Codex", SaveBack: &harnesscapture.Definition{
		Version: 1, ExportCommand: []string{"export"}, HandoffCommand: []string{"handoff"}, Settings: []harnesscapture.Setting{}, Watch: []string{"~/.codex"},
		Providers: []harnesscapture.Provider{{ID: "vendor", EnvName: "API_KEY", Types: []string{"token"},
			Routes: []harnesscapture.Route{{Hosts: []string{"api.vendor.example"}}, {Hosts: []string{"api.vendor.example"}, Paths: []string{"/admin"}, Blocked: true}}}},
	}}
	body, err := svcapi.Convert[apimodel.HarnessConfig](config)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	saveBack, ok := body.SaveBack.Get()
	if !ok || len(saveBack.Providers) != 1 {
		t.Fatalf("save-back = %+v, want its provider", body.SaveBack)
	}
	if routes, _ := saveBack.Providers[0].Routes.Get(); len(routes) != 2 || !routes[1].Blocked.Or(false) {
		t.Fatalf("save-back = %+v, want the blocked route carried", body.SaveBack)
	}
}
