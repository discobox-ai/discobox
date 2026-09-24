package handlers

import (
	"context"
	"sort"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// Host trust (ADR 0149): the project's trust-request inbox and a sandbox's
// trusts, for people; the relay of an agent's ask and the pool's read of what
// its proxy enforces, for pool agents.

func (h *Handler) ListTrustRequests(ctx context.Context, params serverapi.ListTrustRequestsParams) (serverapi.ListTrustRequestsRes, error) {
	status := ""
	if v, ok := params.Status.Get(); ok {
		status = string(v)
	}
	reqs, err := h.services.HostTrusts.ListTrustRequests(ctx, params.ProjectId, status)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListTrustRequestsBody](struct {
		TrustRequests any `json:"trustRequests"`
	}{TrustRequests: reqs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetTrustRequest(ctx context.Context, params serverapi.GetTrustRequestParams) (serverapi.GetTrustRequestRes, error) {
	req, err := h.services.HostTrusts.GetTrustRequest(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HostTrustRequest](req)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ApproveTrustRequest(ctx context.Context, req *apimodel.ApproveTrustRequestBody, params serverapi.ApproveTrustRequestParams) (serverapi.ApproveTrustRequestRes, error) {
	result, err := h.services.HostTrusts.ApproveTrustRequest(ctx, params.ProjectId, params.RequestId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HostTrustRequest](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DenyTrustRequest(ctx context.Context, params serverapi.DenyTrustRequestParams) (serverapi.DenyTrustRequestRes, error) {
	if err := h.services.HostTrusts.DenyTrustRequest(ctx, params.ProjectId, params.RequestId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DenyTrustRequestNoContent{}, nil
}

func (h *Handler) ListSandboxHostTrusts(ctx context.Context, params serverapi.ListSandboxHostTrustsParams) (serverapi.ListSandboxHostTrustsRes, error) {
	trusts, err := h.services.HostTrusts.ListSandboxHostTrusts(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	return hostTrustsBody(trusts)
}

func (h *Handler) DeleteSandboxHostTrust(ctx context.Context, params serverapi.DeleteSandboxHostTrustParams) (serverapi.DeleteSandboxHostTrustRes, error) {
	if err := h.services.HostTrusts.DeleteSandboxHostTrust(ctx, params.ProjectId, params.SandboxId, params.TrustId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSandboxHostTrustNoContent{}, nil
}

// ListApprovalRequests is the window's inbox: a project's credential and
// trust requests in one read, newest first (ADR 0149 §7). It is a read model
// and nothing else — each item is answered on its own resource's routes — so
// it composes the two resources' own listings rather than owning any state.
func (h *Handler) ListApprovalRequests(ctx context.Context, params serverapi.ListApprovalRequestsParams) (serverapi.ListApprovalRequestsRes, error) {
	status := ""
	if v, ok := params.Status.Get(); ok {
		status = string(v)
	}
	credentials, err := h.services.Secrets.ListSecretRequests(ctx, params.ProjectId, status)
	if err != nil {
		return apiError(err), nil
	}
	trusts, err := h.services.HostTrusts.ListTrustRequests(ctx, params.ProjectId, status)
	if err != nil {
		return apiError(err), nil
	}
	type dated struct {
		at   time.Time
		item apimodel.ApprovalRequest
	}
	items := make([]dated, 0, len(credentials)+len(trusts))
	for i := range credentials {
		credential, err := services.Convert[apimodel.SecretRequest](credentials[i])
		if err != nil {
			return nil, err
		}
		items = append(items, dated{at: credentials[i].CreatedAt, item: apimodel.ApprovalRequest{
			Kind:       serverapi.ApprovalRequestKindCredential,
			Credential: serverapi.NewOptSecretRequest(credential),
		}})
	}
	for i := range trusts {
		trust, err := services.Convert[apimodel.HostTrustRequest](trusts[i])
		if err != nil {
			return nil, err
		}
		items = append(items, dated{at: trusts[i].CreatedAt, item: apimodel.ApprovalRequest{
			Kind:  serverapi.ApprovalRequestKindTrust,
			Trust: serverapi.NewOptHostTrustRequest(trust),
		}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	body := &apimodel.ListApprovalRequestsBody{ApprovalRequests: make([]apimodel.ApprovalRequest, 0, len(items))}
	for _, item := range items {
		body.ApprovalRequests = append(body.ApprovalRequests, item.item)
	}
	return body, nil
}

// The pool half. Each call is a pool agent relaying for one of its own
// sandboxes, authorized as the credential broker is: the trust ask is the
// broker's work, done on the sandbox's behalf.

func (h *Handler) CreateSandboxTrustRequest(ctx context.Context, req *apimodel.CreateSandboxTrustRequestBody, _ serverapi.CreateSandboxTrustRequestParams) (serverapi.CreateSandboxTrustRequestRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	result, err := h.services.HostTrusts.CreateSandboxTrustRequest(ctx, principal.PoolID, *req)
	if err != nil {
		return apiError(err), nil
	}
	return sandboxTrustRequestStatus(result, nil), nil
}

func (h *Handler) GetSandboxTrustRequest(ctx context.Context, params serverapi.GetSandboxTrustRequestParams) (serverapi.GetSandboxTrustRequestRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	result, trust, err := h.services.HostTrusts.GetSandboxTrustRequest(ctx, principal.PoolID, params.SandboxId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	return sandboxTrustRequestStatus(result, trust), nil
}

func (h *Handler) ListPoolHostTrusts(ctx context.Context, _ serverapi.ListPoolHostTrustsParams) (serverapi.ListPoolHostTrustsRes, error) {
	principal, err := credentialBrokerPrincipal(ctx)
	if err != nil {
		return apiError(err), nil
	}
	trusts, err := h.services.HostTrusts.ListPoolHostTrusts(ctx, principal.PoolID)
	if err != nil {
		return apiError(err), nil
	}
	return hostTrustsBody(trusts)
}

func hostTrustsBody(trusts []model.HostTrust) (*apimodel.ListHostTrustsBody, error) {
	if trusts == nil {
		trusts = []model.HostTrust{}
	}
	body, err := services.Convert[apimodel.ListHostTrustsBody](struct {
		HostTrusts any `json:"hostTrusts"`
	}{HostTrusts: trusts})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

// sandboxTrustRequestStatus maps a request onto the protocol's vocabulary, as
// agentCredentialRequestStatus does a credential's: approved is granted only
// while the trust it minted is live, and denied once it is not.
func sandboxTrustRequestStatus(req *model.HostTrustRequest, trust *model.HostTrust) *apimodel.SandboxTrustRequestStatus {
	resp := &apimodel.SandboxTrustRequestStatus{
		RequestId: req.ID,
		Host:      req.Host,
		Status:    serverapi.SandboxTrustRequestStatusStatusPending,
	}
	switch {
	case req.Status == model.HostTrustRequestStatusApproved && trust != nil:
		resp.Status = serverapi.SandboxTrustRequestStatusStatusGranted
		resp.SetPin(serverapi.NewOptTrustPin(apimodel.TrustPin{Kind: serverapi.TrustPinKind(trust.Pin.Kind), SHA256: trust.Pin.SHA256}))
		resp.SetUses(serverapi.NewOptNilSecretUseArray(apiSecretUses(trust.Uses)))
	case req.Status != model.HostTrustRequestStatusPending:
		resp.Status = serverapi.SandboxTrustRequestStatusStatusDenied
	}
	return resp
}
