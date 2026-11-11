package handlers

import (
	"context"
	"net/http"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
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

func (h *Handler) RefreshSecret(ctx context.Context, req *apimodel.RefreshSecretBody, params serverapi.RefreshSecretParams) (serverapi.RefreshSecretRes, error) {
	sec, err := h.services.Secrets.RefreshSecret(ctx, params.ProjectId, params.SecretId, *req)
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

// ReportSandboxSecretRejection takes a pool agent's report about a credential
// an upstream refused (ADR 0132). It is authorized by the same scope as
// resolving one: the caller is saying what happened to a value it was already
// entitled to ask for, and names nothing it was not already told.
func (h *Handler) ReportSandboxSecretRejection(ctx context.Context, req *apimodel.ReportSandboxSecretRejectionBody, _ serverapi.ReportSandboxSecretRejectionParams) (serverapi.ReportSandboxSecretRejectionRes, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool {
		return apiError(apperrors.NewStatusError(http.StatusUnauthorized, "pool agent authentication required")), nil
	}
	if !principal.HasScope(poolauth.ScopeSecretResolve) {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "secret:resolve scope required")), nil
	}
	err := h.services.Secrets.RecordSandboxSecretRejection(
		ctx,
		principal.PoolID,
		req.SandboxId,
		req.Sentinel,
		req.Host,
		string(req.Outcome),
		req.ValueGeneration.Or(""),
	)
	if err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReportSandboxSecretRejectionNoContent{}, nil
}

// ListSecretRejections is the window's read of the same records: which
// credentials this project cannot use, and what to do about each.
func (h *Handler) ListSecretRejections(ctx context.Context, params serverapi.ListSecretRejectionsParams) (serverapi.ListSecretRejectionsRes, error) {
	rejections, err := h.services.Secrets.ListSecretRejections(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListSecretRejectionsBody](struct {
		SecretRejections any `json:"secretRejections"`
	}{SecretRejections: rejections})
	if err != nil {
		return nil, err
	}
	return &body, nil
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
		resp.SetSecretId(serverapi.NewOptString(resolution.SecretID))
		resp.SetValueGeneration(serverapi.NewOptString(resolution.ValueGeneration))
		for _, rule := range resolution.ReportScope {
			resp.ReportScope = append(resp.ReportScope, apimodel.SecretReportRule{Host: rule.Host, Paths: rule.Paths})
		}
	}
	return &resp, nil
}

// ResolveSandboxRawSecret hands a pool the value of one of its sandbox's raw
// bindings (ADR 26-10-27-840), under the same principal and scope as a swap.
func (h *Handler) ResolveSandboxRawSecret(ctx context.Context, req *apimodel.ResolveSandboxRawSecretBody, _ serverapi.ResolveSandboxRawSecretParams) (serverapi.ResolveSandboxRawSecretRes, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool {
		return apiError(apperrors.NewStatusError(http.StatusUnauthorized, "pool agent authentication required")), nil
	}
	if !principal.HasScope(poolauth.ScopeSecretResolve) {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "secret:resolve scope required")), nil
	}
	resolution, err := h.services.Secrets.ResolveSandboxRawSecret(ctx, principal.PoolID, req.SandboxId, req.Sentinel)
	if err != nil {
		return apiError(err), nil
	}
	resp := apimodel.ResolveSandboxRawSecretResponse{Status: serverapi.ResolveSandboxRawSecretResponseStatusDenied}
	if resolution.Status == model.SecretRequestStatusApproved && resolution.Value != nil && resolution.Value.Token != "" {
		resp.Status = serverapi.ResolveSandboxRawSecretResponseStatusApproved
		resp.SetValue(serverapi.NewOptString(resolution.Value.Token))
		if resolution.ExpiresAt != nil {
			resp.SetExpiresAt(serverapi.NewOptDateTime(*resolution.ExpiresAt))
		}
		resp.SetSecretId(serverapi.NewOptString(resolution.SecretID))
		resp.SetBindingId(serverapi.NewOptString(resolution.BindingID))
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
			Name:      credential.Name,
			EnvVar:    credential.EnvName,
			Hosts:     credential.Hosts,
			SecretId:  credential.SecretID,
			BindingId: credential.BindingID,
			Purpose:   serverapi.NewOptSandboxCredentialPurpose(serverapi.SandboxCredentialPurpose(purposeOf(credential.Purpose))),
		}
		if credential.Raw != "" {
			item.SetRaw(serverapi.NewOptSandboxCredentialRaw(serverapi.SandboxCredentialRaw(credential.Raw)))
		}
		if item.Hosts == nil {
			item.Hosts = []string{}
		}
		if credential.ExpiresAt != nil {
			item.SetExpiresAt(serverapi.NewOptDateTime(*credential.ExpiresAt))
		}
		for _, grant := range credential.Grants {
			held := apimodel.SandboxCredentialGrant{BindingId: grant.BindingID, Hosts: grant.Hosts}
			if held.Hosts == nil {
				held.Hosts = []string{}
			}
			if grant.Adds {
				held.Adds = serverapi.NewOptBool(true)
			}
			if grant.ExpiresAt != nil {
				held.ExpiresAt = serverapi.NewOptDateTime(*grant.ExpiresAt)
			}
			item.Grants = append(item.Grants, held)
		}
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
	return agentCredentialRequestStatus(result, services.RequestBindings{}, nil), nil
}

func (h *Handler) GetSandboxCredentialRequest(ctx context.Context, params serverapi.GetSandboxCredentialRequestParams) (serverapi.GetSandboxCredentialRequestRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	result, bindings, err := h.services.Secrets.GetSandboxCredentialRequest(ctx, principal.PoolID, params.SandboxId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	// A granted credential is listed under its secret's name, so the status
	// names it the same way.
	var secret *model.Secret
	if result.SecretID != "" && (bindings.Use != nil || bindings.Delegate != nil) {
		if found, err := h.services.Secrets.GetSecret(ctx, result.ProjectID, result.SecretID); err == nil {
			secret = found
		}
	}
	return agentCredentialRequestStatus(result, bindings, secret), nil
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

// agentCredentialRequestStatus is a request as the protocol reports it, part
// by part (ADR 26-11-02-022 §4). Once a part is granted it carries what the
// sandbox now holds under it, as the approver chose it: the hosts the open
// routes of the binding name — else its secret's — and its lifetime, and for
// the use part the variable its sentinel is in. The credential is named after
// its secret, as list names it.
func agentCredentialRequestStatus(req *model.SecretRequest, bindings services.RequestBindings, secret *model.Secret) *apimodel.SandboxCredentialRequestStatus {
	resp := &apimodel.SandboxCredentialRequestStatus{
		RequestId: req.ID,
		Status:    serverapi.SandboxCredentialRequestStatusStatus(services.AgentCredentialRequestStatus(req, bindings)),
	}
	if req.Use.Asked {
		resp.SetUse(serverapi.NewOptSandboxCredentialRequestPartStatus(agentCredentialPartStatus(req, req.Use, bindings.Use, secret)))
	}
	if req.Delegate.Asked {
		resp.SetDelegate(serverapi.NewOptSandboxCredentialRequestPartStatus(agentCredentialPartStatus(req, req.Delegate, bindings.Delegate, secret)))
	}
	return resp
}

func agentCredentialPartStatus(req *model.SecretRequest, part model.SecretRequestPart, binding *model.SecretBinding, secret *model.Secret) apimodel.SandboxCredentialRequestPartStatus {
	status := apimodel.SandboxCredentialRequestPartStatus{
		Status: serverapi.SandboxCredentialRequestPartStatusStatus(services.AgentCredentialPartStatus(part, binding)),
	}
	if binding == nil {
		return status
	}
	name := req.Name
	if secret != nil {
		name = secret.Name
	}
	status.SetName(serverapi.NewOptString(name))
	routes := binding.Routes
	if len(routes) == 0 && secret != nil {
		routes = secret.Routes
	}
	hosts := secretroute.OpenRouteHosts(routes)
	if hosts == nil {
		hosts = []string{}
	}
	status.SetHosts(hosts)
	if binding.Use && binding.EnvName != "" {
		status.SetEnvVar(serverapi.NewOptString(binding.EnvName))
	}
	if binding.Use && binding.Raw != "" {
		status.SetRaw(serverapi.NewOptSandboxCredentialRequestPartStatusRaw(serverapi.SandboxCredentialRequestPartStatusRaw(binding.Raw)))
	}
	// An agent that asked to add and was given a replacement has lost what
	// it did not restate, so how it was granted is said (ADR 26-11-03-329 §4).
	if binding.Use && part.ApprovedAs != "" {
		status.SetGrantedAs(serverapi.NewOptSandboxCredentialRequestPartStatusGrantedAs(serverapi.SandboxCredentialRequestPartStatusGrantedAs(part.ApprovedAs)))
	}
	if binding.ExpiresAt != nil {
		status.SetExpiresAt(serverapi.NewOptDateTime(*binding.ExpiresAt))
	}
	return status
}

// purposeOf is a binding's or a request's purpose as the API states it: an
// empty one asks to use, as it does everywhere a purpose is read.
func purposeOf(purpose string) string {
	if purpose == "" {
		return model.SecretPurposeUse
	}
	return purpose
}

func (h *Handler) ListSecretRefreshes(ctx context.Context, params serverapi.ListSecretRefreshesParams) (serverapi.ListSecretRefreshesRes, error) {
	filter := services.SecretRefreshFilter{
		ID:        params.ID.Or(""),
		SandboxID: params.SandboxId.Or(""),
		SecretID:  params.SecretId.Or(""),
		Since:     params.Since.Or(time.Time{}),
		Until:     params.Until.Or(time.Time{}),
		Ascending: params.Order.Or(serverapi.ListSecretRefreshesOrderDesc) == serverapi.ListSecretRefreshesOrderAsc,
		Limit:     params.Limit.Or(100),
	}
	events, err := h.services.Secrets.ListSecretRefreshEvents(ctx, params.ProjectId, filter)
	if err != nil {
		return apiError(err), nil
	}
	if events == nil {
		// A nil slice encodes as null, which the required array refuses.
		events = []model.SecretRefreshEvent{}
	}
	body, err := services.Convert[apimodel.ListSecretRefreshEventsBody](struct {
		SecretRefreshEvents any `json:"secretRefreshEvents"`
	}{SecretRefreshEvents: events})
	if err != nil {
		return nil, err
	}
	return &body, nil
}
