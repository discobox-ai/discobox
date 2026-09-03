package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
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

func (fakeSecretService) ListSandboxCredentials(context.Context, string, string) ([]store.AgentCredential, error) {
	return nil, nil
}

func (fakeSecretService) CreateSandboxCredentialRequest(context.Context, string, svcapi.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil
}

func (fakeSecretService) GetSandboxCredentialRequest(context.Context, string, string, string) (*model.SecretRequest, *model.SecretGrant, error) {
	return &model.SecretRequest{ID: "sreq-1", Status: model.SecretRequestStatusPending}, nil, nil
}

func (fakeSecretService) RecordCredentialVerdict(context.Context, string, svcapi.RecordCredentialVerdictBody) error {
	return nil
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
		Host:        "github.com",
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
		Host:      "github.com",
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
