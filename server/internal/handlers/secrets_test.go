package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
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

	createRes, err := h.CreateSecretRequest(context.Background(), &serverapi.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeToken,
	}, serverapi.CreateSecretRequestParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("create secret request: %v", err)
	}
	assertResponseDoesNotContain(t, createRes, "clear-token")
	assertResponseDoesNotContain(t, createRes, "value")
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

func (fakeSecretService) CreateSecretRequest(context.Context, string, svcapi.CreateSecretRequestBody) (*model.SecretRequest, error) {
	request := fakeSecretRequest()
	return &request, nil
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

func (fakeSecretService) ListSecretGrants(context.Context, string, string) ([]model.SecretGrant, error) {
	return []model.SecretGrant{fakeSecretGrant()}, nil
}

func (fakeSecretService) CreateSecretGrant(context.Context, string, svcapi.CreateSecretGrantBody) (*model.SecretGrant, error) {
	grant := fakeSecretGrant()
	return &grant, nil
}

func (fakeSecretService) RevokeSecretGrant(context.Context, string, string) error {
	return nil
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

func (fakeSecretService) GetSandboxCredentialRequest(context.Context, string, string, string) (*model.SecretRequest, *model.SecretGrant, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil, nil
}

func fakeSecret() model.Secret {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.Secret{
		ID:             "secret-1",
		ProjectID:      "project-1",
		Name:           "github",
		Type:           model.SecretTypeToken,
		Host:           "github.com",
		MaxGrantTTL:    3600,
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
		Type:        model.SecretTypeToken,
		Hosts:       []string{"api.github.com", "api.githubcopilot.com"},
		SecretID:    "secret-1",
		Status:      model.SecretRequestStatusApproved,
		GrantID:     "grant-1",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func fakeSecretGrant() model.SecretGrant {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretGrant{
		ID:        "grant-1",
		ProjectID: "project-1",
		SecretID:  "secret-1",
		Scope:     model.SecretGrantScopeProject,
		ScopeKey:  "project-1",
		Hosts:     []string{"api.github.com", "api.githubcopilot.com"},
		GrantedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
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

// A grant and a request carry their hosts (ADR 26-10-02-393).
func TestSecretGrantsAndRequestsCarryTheirHosts(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})
	ctx := context.Background()
	want := []string{"api.github.com", "api.githubcopilot.com"}

	listRes, err := h.ListSecretGrants(ctx, serverapi.ListSecretGrantsParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	grants := listRes.(*serverapi.ListSecretGrantsBody).SecretGrants
	if len(grants) != 1 || !slices.Equal(grants[0].Hosts, want) {
		t.Fatalf("grants = %+v, want hosts %q", grants, want)
	}

	getRes, err := h.GetSecretRequest(ctx, serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get request: %v", err)
	}
	if request := getRes.(*serverapi.SecretRequest); !slices.Equal(request.Hosts, want) {
		t.Fatalf("request = %+v, want hosts %q", request, want)
	}
}

// The approver picks a grant's hosts and lifetime, so the status the asking
// sandbox polls says what was picked: the hosts, the variable its sentinel is
// in, and an expiry, or none for a grant that never lapses. A delegation is
// in no variable.
func TestSandboxCredentialRequestStatusCarriesWhatWasGranted(t *testing.T) {
	approved := &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusApproved, Name: "asked", EnvName: "GH_TOKEN"}
	expires := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	timed := agentCredentialRequestStatus(approved, &model.SecretGrant{Hosts: []string{"api.github.com"}, ExpiresAt: &expires}, "github")
	if got, ok := timed.ExpiresAt.Get(); !ok || !got.Equal(expires) {
		t.Fatalf("expiresAt = %v (set %v), want %v", got, ok, expires)
	}
	if timed.Name.Or("") != "github" || timed.EnvVar.Or("") != "GH_TOKEN" || !slices.Equal(timed.Hosts, []string{"api.github.com"}) ||
		timed.Status != serverapi.SandboxCredentialRequestStatusStatusGranted || timed.Purpose != serverapi.SandboxCredentialRequestStatusPurposeUse {
		t.Fatalf("status = %+v, want the grant's name, variable and hosts", timed)
	}

	forever := agentCredentialRequestStatus(approved, &model.SecretGrant{}, "github")
	if forever.ExpiresAt.IsSet() {
		t.Fatalf("expiresAt = %v on a grant that never expires, want it absent", forever.ExpiresAt)
	}

	delegated := &model.SecretRequest{ID: "sreq-2", Status: model.SecretRequestStatusApproved, Name: "github", Purpose: model.SecretGrantPurposeDelegate}
	status := agentCredentialRequestStatus(delegated, &model.SecretGrant{Purpose: model.SecretGrantPurposeDelegate, Hosts: []string{"github.com"}}, "github")
	if status.EnvVar.IsSet() || status.Purpose != serverapi.SandboxCredentialRequestStatusPurposeDelegate {
		t.Fatalf("status = %+v, want a delegation in no variable", status)
	}

	pending := agentCredentialRequestStatus(&model.SecretRequest{ID: "sreq-3", Status: model.SecretRequestStatusPending, EnvName: "X"}, nil, "")
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
		{Name: "github", EnvName: "GH_TOKEN", SecretID: "sec-1", GrantID: "g-1", Hosts: []string{"api.github.com"}, Purpose: model.SecretGrantPurposeUse},
		{Name: "github", SecretID: "sec-1", GrantID: "g-2", Hosts: []string{"github.com"}, Purpose: model.SecretGrantPurposeDelegate},
	}, nil
}
