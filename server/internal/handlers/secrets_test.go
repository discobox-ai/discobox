package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/model"
	svcapi "github.com/obot-platform/discobox/server/internal/services"
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

func TestSecretRequestHandlerCanReturnSecretValue(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	res, err := h.GetSecretRequest(context.Background(), serverapi.GetSecretRequestParams{ProjectId: "project-1", RequestId: "request-1"})
	if err != nil {
		t.Fatalf("get secret request: %v", err)
	}
	assertResponseContains(t, res, "clear-token")
	assertResponseContains(t, res, "value")
}

func TestCreateSecretRequestHandlerCanReturnAutoApprovedSecretValue(t *testing.T) {
	h := New(svcapi.Services{Secrets: fakeSecretService{}})

	res, err := h.CreateSecretRequest(context.Background(), &serverapi.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeBearer,
	}, serverapi.CreateSecretRequestParams{ProjectId: "project-1"})
	if err != nil {
		t.Fatalf("create secret request: %v", err)
	}
	assertResponseContains(t, res, "clear-token")
	assertResponseContains(t, res, "value")
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

func (fakeSecretService) ListSecretRequests(context.Context, string) ([]model.SecretRequest, error) {
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

func fakeSecret() model.Secret {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.Secret{
		ID:              "secret-1",
		ProjectID:       "project-1",
		Name:            "github",
		Type:            model.SecretTypeBearer,
		Host:            "github.com",
		AutoApprove:     true,
		DefaultGrantTTL: 3600,
		EncryptedValue:  []byte(`{"token":"encrypted-token"}`),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func fakeSecretRequest() model.SecretRequest {
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	return model.SecretRequest{
		ID:          "request-1",
		ProjectID:   "project-1",
		RequestedBy: "user-1",
		Type:        model.SecretTypeBearer,
		Host:        "github.com",
		SecretID:    "secret-1",
		Status:      model.SecretRequestStatusApproved,
		CreatedAt:   now,
		UpdatedAt:   now,
		Value:       &model.SecretValue{Token: "clear-token"},
	}
}

func assertResponseContains(t *testing.T, res any, needle string) {
	t.Helper()
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(data), needle) {
		t.Fatalf("response = %s, want %q", data, needle)
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
