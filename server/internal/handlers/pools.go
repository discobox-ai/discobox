package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/auditid"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) ListPools(ctx context.Context, params serverapi.ListPoolsParams) (serverapi.ListPoolsRes, error) {
	pools, err := h.services.Pools.ListPools(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body := apimodel.ListPoolsBody{Pools: make([]apimodel.Pool, 0, len(pools))}
	for i := range pools {
		pool, err := services.PoolToAPI(&pools[i])
		if err != nil {
			return nil, err
		}
		body.Pools = append(body.Pools, pool)
	}
	return &body, nil
}

func (h *Handler) CreatePool(ctx context.Context, req *apimodel.CreatePoolBody, params serverapi.CreatePoolParams) (serverapi.CreatePoolRes, error) {
	pool, err := h.services.Pools.CreatePool(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.PoolToAPI(pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetPool(ctx context.Context, params serverapi.GetPoolParams) (serverapi.GetPoolRes, error) {
	pool, err := h.services.Pools.GetPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.PoolToAPI(pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdatePool(ctx context.Context, req *apimodel.UpdatePoolBody, params serverapi.UpdatePoolParams) (serverapi.UpdatePoolRes, error) {
	pool, err := h.services.Pools.UpdatePool(ctx, params.ProjectId, params.PoolId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.PoolToAPI(pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeletePool(ctx context.Context, params serverapi.DeletePoolParams) (serverapi.DeletePoolRes, error) {
	if err := h.services.Pools.DeletePool(ctx, params.ProjectId, params.PoolId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeletePoolNoContent{}, nil
}

func (h *Handler) SetDefaultPool(ctx context.Context, params serverapi.SetDefaultPoolParams) (serverapi.SetDefaultPoolRes, error) {
	project, err := h.services.Pools.SetDefaultPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UnsetDefaultPool(ctx context.Context, params serverapi.UnsetDefaultPoolParams) (serverapi.UnsetDefaultPoolRes, error) {
	project, err := h.services.Pools.UnsetDefaultPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ReconcilePool(ctx context.Context, params serverapi.ReconcilePoolParams) (serverapi.ReconcilePoolRes, error) {
	pool, err := h.services.Pools.ReconcilePool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.PoolToAPI(pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListHTTPAudit(ctx context.Context, params serverapi.ListHTTPAuditParams) (serverapi.ListHTTPAuditRes, error) {
	filter := services.HTTPAuditFilter{
		SandboxID: params.SandboxId.Or(""),
		PoolID:    params.PoolId.Or(""),
		Host:      params.Host.Or(""),
		UseID:     params.UseId.Or(""),
		Since:     params.Since.Or(time.Time{}),
		Until:     params.Until.Or(time.Time{}),
		MinStatus: params.MinStatus.Or(0),
		MaxStatus: params.MaxStatus.Or(0),
		Ascending: params.Order.Or(serverapi.ListHTTPAuditOrderDesc) == serverapi.ListHTTPAuditOrderAsc,
		Limit:     params.Limit.Or(100),
	}
	if blocked, ok := params.Blocked.Get(); ok {
		filter.Blocked = &blocked
	}
	after, err := parsePoolCursors(params.After, auditid.ExchangePrefix, auditid.ParseExchange)
	if err != nil {
		return apiError(err), nil
	}
	filter.After = after
	result, err := h.services.Pools.ListHTTPAudit(ctx, params.ProjectId, filter)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListHTTPAuditBody](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

// ListDNSAudit reads the DNS queries the project's pools answered (ADR 0148).
func (h *Handler) ListDNSAudit(ctx context.Context, params serverapi.ListDNSAuditParams) (serverapi.ListDNSAuditRes, error) {
	after, err := parsePoolCursors(params.After, auditid.DNSPrefix, auditid.ParseDNSQuery)
	if err != nil {
		return apiError(err), nil
	}
	var id auditid.DNSQueryID
	if raw := params.ID.Or(""); raw != "" {
		if id, err = auditid.ParseDNSQuery(raw); err != nil {
			return apiError(apperrors.NewStatusError(http.StatusBadRequest, err.Error())), nil
		}
	}
	result, err := h.services.Pools.ListDNSAudit(ctx, params.ProjectId, services.DNSAuditFilter{
		ID:        id,
		SandboxID: params.SandboxId.Or(""),
		PoolID:    params.PoolId.Or(""),
		Name:      params.Name.Or(""),
		Since:     params.Since.Or(time.Time{}),
		Until:     params.Until.Or(time.Time{}),
		Ascending: params.Order.Or(serverapi.ListDNSAuditOrderDesc) == serverapi.ListDNSAuditOrderAsc,
		After:     after,
		Limit:     params.Limit.Or(100),
	})
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListDNSAuditBody](result)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

// GetHTTPAudit reads one audited exchange in full (ADR 0130 §5).
func (h *Handler) GetHTTPAudit(ctx context.Context, params serverapi.GetHTTPAuditParams) (serverapi.GetHTTPAuditRes, error) {
	id, err := auditid.ParseExchange(params.ExchangeId)
	if err != nil {
		return apiError(apperrors.NewStatusError(http.StatusBadRequest, err.Error())), nil
	}
	detail, err := h.services.Pools.GetHTTPAudit(ctx, params.ProjectId, params.PoolId, params.SandboxId.Or(""), id)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HTTPAuditExchangeDetail](detail)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ClearPoolCache(ctx context.Context, params serverapi.ClearPoolCacheParams) (serverapi.ClearPoolCacheRes, error) {
	stopped, err := h.services.Pools.ClearPoolCache(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	if stopped == nil {
		stopped = []string{}
	}
	return &apimodel.ClearPoolCacheBody{StoppedSandboxIds: stopped}, nil
}

func (h *Handler) RegisterPool(ctx context.Context, req *apimodel.RegisterPoolBody) (serverapi.RegisterPoolRes, error) {
	resp, err := h.services.Pools.RegisterPool(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.RegisterPoolResponseBody](resp)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdatePoolStatus(ctx context.Context, req *apimodel.UpdatePoolStatusBody, params serverapi.UpdatePoolStatusParams) (serverapi.UpdatePoolStatusRes, error) {
	pool, err := h.services.Pools.UpdatePoolStatus(ctx, params.PoolId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.PoolToAPI(pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ReportPoolSandboxStates(ctx context.Context, req *apimodel.ReportPoolSandboxStatesBody, params serverapi.ReportPoolSandboxStatesParams) (serverapi.ReportPoolSandboxStatesRes, error) {
	if err := h.services.Pools.ReportPoolSandboxStates(ctx, params.PoolId, *req); err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReportPoolSandboxStatesNoContent{}, nil
}

func (h *Handler) ReportSandboxAgentStatus(ctx context.Context, req *apimodel.ReportSandboxAgentStatusBody, params serverapi.ReportSandboxAgentStatusParams) (serverapi.ReportSandboxAgentStatusRes, error) {
	if err := h.services.Pools.ReportSandboxAgentStatus(ctx, params.PoolId, *req); err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReportSandboxAgentStatusNoContent{}, nil
}

func (h *Handler) ReportPoolResources(ctx context.Context, req *apimodel.ReportPoolResourcesBody, params serverapi.ReportPoolResourcesParams) (serverapi.ReportPoolResourcesRes, error) {
	if err := h.services.Pools.ReportPoolResources(ctx, params.PoolId, *req); err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReportPoolResourcesNoContent{}, nil
}

func (h *Handler) MintSandboxAgentStatusTokens(ctx context.Context, req *apimodel.MintSandboxAgentStatusTokensBody, params serverapi.MintSandboxAgentStatusTokensParams) (serverapi.MintSandboxAgentStatusTokensRes, error) {
	resp, err := h.services.Pools.MintSandboxAgentStatusTokens(ctx, params.PoolId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.MintSandboxAgentStatusTokensResponseBody](resp)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

// parsePoolCursors reads an audit list's `after` parameter: one
// `poolId:<prefix><row>` per pool the caller already has records from, parsed
// with the trail's own ID parser so one trail's cursor is refused by another. A
// record ID only means anything on the pool that issued it, so the pool travels
// with it rather than the API pretending one cursor covers a merged read.
func parsePoolCursors[ID ~uint64](values []string, prefix string, parse func(string) (ID, error)) (map[string]ID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	after := make(map[string]ID, len(values))
	for _, value := range values {
		poolID, recordID, ok := strings.Cut(value, ":")
		if !ok || poolID == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("after %q: want poolId:%s<row>", value, prefix))
		}
		id, err := parse(recordID)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("after %q: %s", value, err))
		}
		// The highest id wins, so a repeated pool cannot read a row twice.
		if id > after[poolID] {
			after[poolID] = id
		}
	}
	return after, nil
}
