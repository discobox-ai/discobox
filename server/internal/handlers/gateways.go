package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func (h *Handler) ListProjectGateways(ctx context.Context, params serverapi.ListProjectGatewaysParams) (serverapi.ListProjectGatewaysRes, error) {
	gateways, err := h.services.Gateways.ListProjectGateways(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body := apimodel.ListProjectGatewaysBody{Gateways: make([]apimodel.ProjectGateway, 0, len(gateways))}
	for _, gw := range gateways {
		body.Gateways = append(body.Gateways, projectGatewayToAPI(gw))
	}
	return &body, nil
}

func (h *Handler) GetProjectGateway(ctx context.Context, params serverapi.GetProjectGatewayParams) (serverapi.GetProjectGatewayRes, error) {
	gw, err := h.services.Gateways.GetProjectGateway(ctx, params.ProjectId, params.GatewayName)
	if err != nil {
		return apiError(err), nil
	}
	body := projectGatewayToAPI(*gw)
	return &body, nil
}

func (h *Handler) PutProjectGateway(ctx context.Context, req *apimodel.PutProjectGatewayBody, params serverapi.PutProjectGatewayParams) (serverapi.PutProjectGatewayRes, error) {
	route := gateway.Route{Match: req.Match, Target: req.Target}
	if headers, ok := req.RequestHeaders.Get(); ok {
		route.RequestHeaders = gateway.HeaderModifier{
			Remove: headers.Remove,
			Set:    gatewayHeadersFromAPI(headers.Set),
			Add:    gatewayHeadersFromAPI(headers.Add),
		}
	}
	gw, err := h.services.Gateways.PutProjectGateway(ctx, params.ProjectId, params.GatewayName, route)
	if err != nil {
		return apiError(err), nil
	}
	body := projectGatewayToAPI(*gw)
	return &body, nil
}

func (h *Handler) DeleteProjectGateway(ctx context.Context, params serverapi.DeleteProjectGatewayParams) (serverapi.DeleteProjectGatewayRes, error) {
	if err := h.services.Gateways.DeleteProjectGateway(ctx, params.ProjectId, params.GatewayName); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteProjectGatewayNoContent{}, nil
}

// ResolveGatewayHeaders is a pool's proxy asking for the server environment
// values a sandbox's gateway sets headers from (ADR 26-10-16-470 §4). It is
// authorized as ResolveSandboxSecret is: a pool principal with the resolve
// scope, and the sandbox's pool re-derived in the service.
func (h *Handler) ResolveGatewayHeaders(ctx context.Context, req *apimodel.ResolveGatewayHeadersBody, _ serverapi.ResolveGatewayHeadersParams) (serverapi.ResolveGatewayHeadersRes, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool {
		return apiError(apperrors.NewStatusError(http.StatusUnauthorized, "pool agent authentication required")), nil
	}
	if !principal.HasScope(poolauth.ScopeSecretResolve) {
		return apiError(apperrors.NewStatusError(http.StatusForbidden, "secret:resolve scope required")), nil
	}
	values, err := h.services.Gateways.ResolveGatewayHeaders(ctx, principal.PoolID, req.SandboxId, req.Gateway, req.Env)
	if err != nil {
		return apiError(err), nil
	}
	return &apimodel.ResolveGatewayHeadersResponse{Values: values}, nil
}

func projectGatewayToAPI(gw model.ProjectGateway) apimodel.ProjectGateway {
	out := apimodel.ProjectGateway{
		Name:      gw.Name,
		ProjectId: gw.ProjectID,
		Match:     gw.Match,
		Target:    gw.Target,
		RequestHeaders: apimodel.GatewayHeaderModifier{
			Remove: gw.RequestHeaders.Remove,
			Set:    gatewayHeadersToAPI(gw.RequestHeaders.Set),
			Add:    gatewayHeadersToAPI(gw.RequestHeaders.Add),
		},
		ManagedBy: serverapi.ProjectGatewayManagedBy(gw.ManagedBy),
		CreatedAt: gw.CreatedAt,
		UpdatedAt: gw.UpdatedAt,
	}
	if gw.CreatedBy != "" {
		out.CreatedBy = serverapi.NewOptString(gw.CreatedBy)
	}
	return out
}

func gatewayHeadersToAPI(headers []gateway.Header) []apimodel.GatewayHeader {
	var out []apimodel.GatewayHeader
	for _, header := range headers {
		item := apimodel.GatewayHeader{Name: header.Name}
		if header.Env != "" {
			item.ValueFrom = serverapi.NewOptGatewayHeaderValueSource(apimodel.GatewayHeaderValueSource{Env: header.Env})
		} else {
			item.Value = serverapi.NewOptString(header.Value)
		}
		out = append(out, item)
	}
	return out
}

// gatewayHeadersFromAPI reads a put's headers, each a written value: only the
// server configuration file takes one from the environment (ADR 26-10-17-271
// §3).
func gatewayHeadersFromAPI(in []apimodel.PutGatewayHeader) []gateway.Header {
	var out []gateway.Header
	for _, header := range in {
		out = append(out, gateway.Header{Name: header.Name, Value: header.Value})
	}
	return out
}
