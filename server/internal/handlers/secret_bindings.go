package handlers

import (
	"context"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// Bindings are the authorization (ADR 26-10-27-880 §1): a person's to make,
// change and remove, each change recorded as an event.

func (h *Handler) ListSecretBindings(ctx context.Context, params serverapi.ListSecretBindingsParams) (serverapi.ListSecretBindingsRes, error) {
	query := services.SecretBindingQuery{
		Secret:     params.Secret.Or(""),
		TargetKind: string(params.TargetKind.Or("")),
		Target:     params.Target.Or(""),
		ParentID:   params.ParentId.Or(""),
	}
	bindings, err := h.services.Secrets.ListSecretBindings(ctx, params.ProjectId, query)
	if err != nil {
		return apiError(err), nil
	}
	if bindings == nil {
		bindings = []model.SecretBinding{}
	}
	body, err := services.Convert[apimodel.ListSecretBindingsBody](struct {
		SecretBindings any `json:"secretBindings"`
	}{SecretBindings: bindings})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSecretBinding(ctx context.Context, req *apimodel.CreateSecretBindingBody, params serverapi.CreateSecretBindingParams) (serverapi.CreateSecretBindingRes, error) {
	binding, err := h.services.Secrets.CreateSecretBinding(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretBinding](binding)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSecretBinding(ctx context.Context, params serverapi.GetSecretBindingParams) (serverapi.GetSecretBindingRes, error) {
	binding, err := h.services.Secrets.GetSecretBinding(ctx, params.ProjectId, params.BindingId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretBinding](binding)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateSecretBinding(ctx context.Context, req *apimodel.UpdateSecretBindingBody, params serverapi.UpdateSecretBindingParams) (serverapi.UpdateSecretBindingRes, error) {
	binding, err := h.services.Secrets.UpdateSecretBinding(ctx, params.ProjectId, params.BindingId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretBinding](binding)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSecretBinding(ctx context.Context, params serverapi.DeleteSecretBindingParams) (serverapi.DeleteSecretBindingRes, error) {
	deleted, err := h.services.Secrets.DeleteSecretBinding(ctx, params.ProjectId, params.BindingId)
	if err != nil {
		return apiError(err), nil
	}
	if deleted == nil {
		deleted = []string{}
	}
	return &apimodel.DeleteSecretBindingResponse{Deleted: deleted}, nil
}

func (h *Handler) ListSecretBindingEvents(ctx context.Context, params serverapi.ListSecretBindingEventsParams) (serverapi.ListSecretBindingEventsRes, error) {
	query := services.SecretBindingEventQuery{
		BindingID: params.BindingId.Or(""),
		Secret:    params.Secret.Or(""),
		Since:     params.Since.Or(time.Time{}),
		Limit:     int(params.Limit.Or(0)),
	}
	events, err := h.services.Secrets.ListSecretBindingEvents(ctx, params.ProjectId, query)
	if err != nil {
		return apiError(err), nil
	}
	if events == nil {
		events = []model.SecretBindingEvent{}
	}
	body, err := services.Convert[apimodel.ListSecretBindingEventsBody](struct {
		SecretBindingEvents any `json:"secretBindingEvents"`
	}{SecretBindingEvents: events})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

// ListBoundSecrets is the resolved view of a target: what applies there, and
// from where (ADR 26-10-27-880 §2).
func (h *Handler) ListBoundSecrets(ctx context.Context, params serverapi.ListBoundSecretsParams) (serverapi.ListBoundSecretsRes, error) {
	bound, err := h.services.Secrets.ListBoundSecrets(ctx, params.ProjectId, string(params.TargetKind), params.Target.Or(""))
	if err != nil {
		return apiError(err), nil
	}
	body := &apimodel.ListBoundSecretsBody{BoundSecrets: make([]apimodel.BoundSecret, 0, len(bound))}
	for _, item := range bound {
		out := apimodel.BoundSecret{
			BindingId:  item.Binding.ID,
			SecretId:   item.Binding.SecretID,
			SecretName: serverapi.NewOptString(item.SecretName),
			From:       serverapi.BoundSecretFrom(item.Binding.TargetKind),
			FromId:     item.Binding.TargetID,
			Use:        item.Binding.Use,
			Delegate:   item.Binding.Delegate,
			Routes:     make([][]apimodel.SecretRoute, 0, len(item.RouteLists)),
		}
		if item.Binding.Use {
			out.EnvVar = serverapi.NewOptString(item.Binding.EnvName)
		}
		if item.ExpiresAt != nil {
			out.ExpiresAt = serverapi.NewOptDateTime(*item.ExpiresAt)
		}
		for _, list := range item.RouteLists {
			out.Routes = append(out.Routes, services.SecretRoutesFromModel(list))
		}
		body.BoundSecrets = append(body.BoundSecrets, out)
	}
	return body, nil
}

func (h *Handler) ListSecretRefreshRequests(ctx context.Context, params serverapi.ListSecretRefreshRequestsParams) (serverapi.ListSecretRefreshRequestsRes, error) {
	requests, err := h.services.Secrets.ListSecretRefreshRequests(ctx, params.ProjectId, string(params.Status.Or("")))
	if err != nil {
		return apiError(err), nil
	}
	if requests == nil {
		requests = []model.SecretRefreshRequest{}
	}
	body, err := services.Convert[apimodel.ListSecretRefreshRequestsBody](struct {
		SecretRefreshRequests any `json:"secretRefreshRequests"`
	}{SecretRefreshRequests: requests})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DismissSecretRefreshRequest(ctx context.Context, params serverapi.DismissSecretRefreshRequestParams) (serverapi.DismissSecretRefreshRequestRes, error) {
	request, err := h.services.Secrets.DismissSecretRefreshRequest(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SecretRefreshRequest](request)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
