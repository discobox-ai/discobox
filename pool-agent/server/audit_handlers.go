package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/proxy"
)

// AuditReader reads the pool proxy's audit trail. proxy.ControlClient is the
// implementation; the pool agent only relays what it returns.
type AuditReader interface {
	ListHTTP(ctx context.Context, sandboxID string, query proxy.AuditQuery) ([]proxy.AuditHTTPExchange, error)
}

// PoolListHTTPAudit relays the pool proxy's HTTP audit (ADR 0130 §4).
//
// The sandbox the read is narrowed to comes from the token before the query:
// a token the control plane scoped to one sandbox reads that sandbox's rows and
// no one else's, and naming another sandbox in the query is refused rather than
// quietly answered with the token's own.
func (s *sandboxService) PoolListHTTPAudit(ctx context.Context, params workerapi.PoolListHTTPAuditParams) (*workerapimodel.PoolHTTPAuditResponse, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	if s.audit == nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "the pool proxy's audit is not configured")
	}
	sandboxID := params.SandboxId.Or("")
	if claims, ok := SignedTokenClaimsFromContext(ctx); ok && claims.SandboxID != "" {
		if sandboxID != "" && sandboxID != claims.SandboxID {
			return nil, newStatusError(http.StatusForbidden, "token is scoped to another sandbox")
		}
		sandboxID = claims.SandboxID
	}
	rows, err := s.audit.ListHTTP(ctx, sandboxID, proxy.AuditQuery{
		Host:  params.Host.Or(""),
		UseID: params.UseId.Or(""),
		Since: params.Since.Or(time.Time{}),
		Limit: params.Limit.Or(100),
	})
	if err != nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "read the pool proxy's audit: "+err.Error())
	}
	out := &workerapimodel.PoolHTTPAuditResponse{Exchanges: make([]workerapimodel.PoolHTTPAuditExchange, 0, len(rows))}
	for _, row := range rows {
		out.Exchanges = append(out.Exchanges, poolHTTPAuditExchange(row))
	}
	return out, nil
}

func poolHTTPAuditExchange(row proxy.AuditHTTPExchange) workerapimodel.PoolHTTPAuditExchange {
	uses := []string{}
	if row.SwappedUseIDs != "" {
		uses = strings.Split(row.SwappedUseIDs, ",")
	}
	exchange := workerapimodel.PoolHTTPAuditExchange{
		ID:               int64(row.ID),
		CreatedAt:        row.CreatedAt,
		SandboxId:        row.ClientID,
		Method:           row.Method,
		URL:              row.URL,
		Host:             row.Host,
		Status:           row.Status,
		Blocked:          row.Blocked,
		SwappedUseIds:    uses,
		DurationMillis:   workerapi.NewOptInt64(row.DurationMillis),
		CacheHit:         workerapi.NewOptBool(row.CacheHit),
		RequestBodyBytes: workerapi.NewOptInt64(row.RequestBodyBytes),
		ResponseBytes:    workerapi.NewOptInt64(row.ResponseBytes),
		Upgrade:          workerapi.NewOptBool(row.Upgrade),
	}
	if row.BlockedReason != "" {
		exchange.BlockedReason = workerapi.NewOptString(row.BlockedReason)
	}
	if row.UpgradeType != "" {
		exchange.UpgradeType = workerapi.NewOptString(row.UpgradeType)
	}
	return exchange
}
