package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	secretsresource "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
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
	status := ""
	if v, ok := params.Status.Get(); ok {
		status = string(v)
	}
	reqs, err := h.services.Secrets.ListSecretRequests(ctx, params.ProjectId, status)
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
	if !ok || principal.Type != auth.PrincipalTypePool {
		return apiError(apperrors.NewStatusError(http.StatusUnauthorized, "pool agent authentication required")), nil
	}
	if !principal.HasScope(poolauth.ScopeSecretResolve) {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "secret:resolve scope required")), nil
	}
	resolution, err := h.services.Secrets.ResolveSandboxSecret(ctx, principal.PoolID, req.SandboxId, req.Sentinel, req.Host)
	if err != nil {
		return apiError(err), nil
	}
	resp := apimodel.ResolveSandboxSecretResponse{Status: serverapi.ResolveSandboxSecretResponseStatusPending}
	if resolution.Status == model.SecretRequestStatusApproved && resolution.Value != nil && resolution.Value.Token != "" {
		resp.Status = serverapi.ResolveSandboxSecretResponseStatusApproved
		resp.SetValue(serverapi.NewOptString(resolution.Value.Token))
		if resolution.ExpiresAt != nil {
			resp.SetExpiresAt(serverapi.NewOptDateTime(*resolution.ExpiresAt))
		}
	}
	return &resp, nil
}

// The agent credentials broker (ADR 0031). Each route is called by a pool agent
// relaying for one of its own sandboxes, and authorizes the same way
// ResolveSandboxSecret does: a pool principal carrying the broker scope, with
// sandbox ownership re-derived in the service rather than trusted from the body.

func (h *Handler) ListSandboxCredentials(ctx context.Context, params serverapi.ListSandboxCredentialsParams) (serverapi.ListSandboxCredentialsRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	credentials, err := h.services.Secrets.ListSandboxCredentials(ctx, principal.PoolID, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	resp := apimodel.ListSandboxCredentialsResponse{Credentials: make([]apimodel.SandboxCredential, 0, len(credentials))}
	for _, credential := range credentials {
		item := apimodel.SandboxCredential{
			Name:     credential.Name,
			EnvVar:   credential.Assignment.EnvName,
			Host:     credential.Grant.Host,
			SecretId: credential.Assignment.SecretID,
			GrantId:  credential.Grant.ID,
			Sentinel: credential.Assignment.Sentinel,
		}
		if credential.Format != "" {
			item.SetFormat(serverapi.NewOptString(credential.Format))
		}
		if credential.Grant.ExpiresAt != nil {
			item.SetExpiresAt(serverapi.NewOptDateTime(*credential.Grant.ExpiresAt))
		}
		item.SetUses(serverapi.NewOptNilSecretUseArray(apiSecretUses(credential.Grant.Uses)))
		resp.Credentials = append(resp.Credentials, item)
	}
	return &resp, nil
}

func (h *Handler) CreateSandboxCredentialRequest(ctx context.Context, req *apimodel.CreateSandboxCredentialRequestBody, _ serverapi.CreateSandboxCredentialRequestParams) (serverapi.CreateSandboxCredentialRequestRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	result, err := h.services.Secrets.CreateSandboxCredentialRequest(ctx, principal.PoolID, *req)
	if err != nil {
		return apiError(err), nil
	}
	return agentCredentialRequestStatus(result, nil), nil
}

func (h *Handler) GetSandboxCredentialRequest(ctx context.Context, params serverapi.GetSandboxCredentialRequestParams) (serverapi.GetSandboxCredentialRequestRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	result, grant, err := h.services.Secrets.GetSandboxCredentialRequest(ctx, principal.PoolID, params.SandboxId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	return agentCredentialRequestStatus(result, grant), nil
}

// credentialBrokerPrincipal authorizes an agent credentials broker call. The
// scope is distinct from secret:resolve because the two are held by the same
// process for different reasons: resolving is what the proxy does with traffic
// it already has, brokering is what it does on a sandbox's behalf.
func credentialBrokerPrincipal(ctx context.Context) (auth.Principal, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool {
		return auth.Principal{}, apperrors.NewStatusError(http.StatusUnauthorized, "pool agent authentication required")
	}
	if !principal.HasScope(poolauth.ScopeCredentialBroker) {
		return auth.Principal{}, apperrors.NewStatusError(http.StatusForbidden, poolauth.ScopeCredentialBroker+" scope required")
	}
	return principal, nil
}

func agentCredentialRequestStatus(req *model.SecretRequest, grant *model.SecretGrant) *apimodel.SandboxCredentialRequestStatus {
	resp := &apimodel.SandboxCredentialRequestStatus{
		RequestId: req.ID,
		Status:    serverapi.SandboxCredentialRequestStatusStatus(secretsresource.AgentCredentialRequestStatus(req, grant)),
	}
	if grant != nil {
		resp.SetUses(serverapi.NewOptNilSecretUseArray(apiSecretUses(grant.Uses)))
	}
	return resp
}

func apiSecretUses(uses []model.SecretUse) []apimodel.SecretUse {
	out := make([]apimodel.SecretUse, 0, len(uses))
	for _, use := range uses {
		item := apimodel.SecretUse{Description: use.Description}
		if use.UseID != "" {
			item.SetUseId(serverapi.NewOptString(use.UseID))
		}
		out = append(out, item)
	}
	return out
}

func (h *Handler) ListSecretGrants(ctx context.Context, params serverapi.ListSecretGrantsParams) (serverapi.ListSecretGrantsRes, error) {
	grants, err := h.services.Secrets.ListSecretGrants(ctx, params.ProjectId, params.SecretId.Or(""))
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListSecretGrantsBody](struct {
		SecretGrants any `json:"secretGrants"`
	}{SecretGrants: grants})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSecretGrant(ctx context.Context, req *apimodel.CreateSecretGrantBody, params serverapi.CreateSecretGrantParams) (serverapi.CreateSecretGrantRes, error) {
	grant, err := h.services.Secrets.CreateSecretGrant(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretGrant](grant)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RevokeSecretGrant(ctx context.Context, params serverapi.RevokeSecretGrantParams) (serverapi.RevokeSecretGrantRes, error) {
	if err := h.services.Secrets.RevokeSecretGrant(ctx, params.ProjectId, params.GrantId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.RevokeSecretGrantNoContent{}, nil
}
