package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

func (h *Handler) ListSecrets(ctx context.Context, params serverapi.ListSecretsParams) (serverapi.ListSecretsRes, error) {
	secs, err := h.services.Secrets.ListSecrets(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListSecretsBody](struct {
		Secrets any `json:"secrets"`
	}{Secrets: secs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSecret(ctx context.Context, req *apimodel.CreateSecretBody, params serverapi.CreateSecretParams) (serverapi.CreateSecretRes, error) {
	sec, err := h.services.Secrets.CreateSecret(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Secret](sec)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSecret(ctx context.Context, params serverapi.GetSecretParams) (serverapi.GetSecretRes, error) {
	sec, err := h.services.Secrets.GetSecret(ctx, params.ProjectId, params.SecretId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Secret](sec)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateSecret(ctx context.Context, req *apimodel.UpdateSecretBody, params serverapi.UpdateSecretParams) (serverapi.UpdateSecretRes, error) {
	sec, err := h.services.Secrets.UpdateSecret(ctx, params.ProjectId, params.SecretId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Secret](sec)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSecret(ctx context.Context, params serverapi.DeleteSecretParams) (serverapi.DeleteSecretRes, error) {
	if err := h.services.Secrets.DeleteSecret(ctx, params.ProjectId, params.SecretId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSecretNoContent{}, nil
}

func (h *Handler) ListSecretRequests(ctx context.Context, params serverapi.ListSecretRequestsParams) (serverapi.ListSecretRequestsRes, error) {
	reqs, err := h.services.Secrets.ListSecretRequests(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListSecretRequestsBody](struct {
		SecretRequests any `json:"secretRequests"`
	}{SecretRequests: reqs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSecretRequest(ctx context.Context, req *apimodel.CreateSecretRequestBody, params serverapi.CreateSecretRequestParams) (serverapi.CreateSecretRequestRes, error) {
	result, err := h.services.Secrets.CreateSecretRequest(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretRequest](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSecretRequest(ctx context.Context, params serverapi.GetSecretRequestParams) (serverapi.GetSecretRequestRes, error) {
	result, err := h.services.Secrets.GetSecretRequest(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretRequest](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ApproveSecretRequest(ctx context.Context, req *apimodel.ApproveSecretRequestBody, params serverapi.ApproveSecretRequestParams) (serverapi.ApproveSecretRequestRes, error) {
	result, err := h.services.Secrets.ApproveSecretRequest(ctx, params.ProjectId, params.RequestId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretRequest](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DenySecretRequest(ctx context.Context, params serverapi.DenySecretRequestParams) (serverapi.DenySecretRequestRes, error) {
	if err := h.services.Secrets.DenySecretRequest(ctx, params.ProjectId, params.RequestId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DenySecretRequestNoContent{}, nil
}

func (h *Handler) ResolveSandboxSecret(ctx context.Context, req *apimodel.ResolveSandboxSecretBody, _ serverapi.ResolveSandboxSecretParams) (serverapi.ResolveSandboxSecretRes, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeWorker {
		return apiError(apperrors.NewStatusError(http.StatusUnauthorized, "worker authentication required")), nil
	}
	if !principal.HasScope(workerauth.ScopeSecretResolve) {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "secret:resolve scope required")), nil
	}
	request, err := h.services.Secrets.ResolveSandboxSecret(ctx, principal.WorkerID, req.SandboxId, req.Sentinel, req.Host)
	if err != nil {
		return apiError(err), nil
	}
	resp := apimodel.ResolveSandboxSecretResponse{Status: serverapi.ResolveSandboxSecretResponseStatusPending}
	switch request.Status {
	case model.SecretRequestStatusApproved:
		if request.Value != nil && request.Value.Token != "" {
			resp.Status = serverapi.ResolveSandboxSecretResponseStatusApproved
			resp.SetValue(serverapi.NewOptString(request.Value.Token))
			if request.ExpiresAt != nil {
				resp.SetExpiresAt(serverapi.NewOptDateTime(*request.ExpiresAt))
			}
		}
	case model.SecretRequestStatusDenied:
		resp.Status = serverapi.ResolveSandboxSecretResponseStatusDenied
	}
	return &resp, nil
}
