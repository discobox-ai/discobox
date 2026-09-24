package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/go-chi/chi/v5"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/proxy"
)

// AuditReader reads the pool proxy's audit trail. proxy.ControlClient is the
// implementation; the pool agent only relays what it returns.
type AuditReader interface {
	ListHTTP(ctx context.Context, sandboxID string, query proxy.AuditQuery) ([]proxy.AuditHTTPExchange, error)
	GetHTTP(ctx context.Context, sandboxID string, id auditid.ExchangeID) (*proxy.AuditHTTPExchange, error)
	OpenHTTPArtifact(ctx context.Context, sandboxID string, id auditid.ExchangeID, artifact string) (*proxy.AuditArtifact, error)
	ListDNS(ctx context.Context, sandboxID string, query proxy.AuditDNSQueryOptions) ([]proxy.AuditDNSQuery, error)
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
	sandboxID, err := auditSandbox(ctx, params.SandboxId.Or(""))
	if err != nil {
		return nil, err
	}
	query := proxy.AuditQuery{
		Host:      params.Host.Or(""),
		UseID:     params.UseId.Or(""),
		Since:     params.Since.Or(time.Time{}),
		MinStatus: params.MinStatus.Or(0),
		MaxStatus: params.MaxStatus.Or(0),
		Ascending: params.Order.Or(workerapi.PoolListHTTPAuditOrderDesc) == workerapi.PoolListHTTPAuditOrderAsc,
		Limit:     params.Limit.Or(100),
	}
	if blocked, ok := params.Blocked.Get(); ok {
		query.Blocked = &blocked
	}
	if after, ok := params.AfterId.Get(); ok && after != "" {
		id, err := auditid.ParseExchange(after)
		if err != nil {
			return nil, newStatusError(http.StatusBadRequest, err.Error())
		}
		query.AfterID = id
	}
	rows, err := s.audit.ListHTTP(ctx, sandboxID, query)
	if err != nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "read the pool proxy's audit: "+err.Error())
	}
	out := &workerapimodel.PoolHTTPAuditResponse{Exchanges: make([]workerapimodel.PoolHTTPAuditExchange, 0, len(rows))}
	for _, row := range rows {
		out.Exchanges = append(out.Exchanges, poolHTTPAuditExchange(row))
	}
	return out, nil
}

// PoolListDNSAudit relays the DNS queries the pool answered for its sandboxes
// (ADR 0148), narrowed to a sandbox exactly as the HTTP audit is.
func (s *sandboxService) PoolListDNSAudit(ctx context.Context, params workerapi.PoolListDNSAuditParams) (*workerapimodel.PoolDNSAuditResponse, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	if s.audit == nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "the pool proxy's audit is not configured")
	}
	sandboxID, err := auditSandbox(ctx, params.SandboxId.Or(""))
	if err != nil {
		return nil, err
	}
	query := proxy.AuditDNSQueryOptions{
		Name:      params.Name.Or(""),
		Since:     params.Since.Or(time.Time{}),
		Ascending: params.Order.Or(workerapi.PoolListDNSAuditOrderDesc) == workerapi.PoolListDNSAuditOrderAsc,
		Limit:     params.Limit.Or(100),
	}
	for value, field := range map[string]*auditid.DNSQueryID{params.AfterId.Or(""): &query.AfterID, params.ID.Or(""): &query.ID} {
		if value == "" {
			continue
		}
		id, err := auditid.ParseDNSQuery(value)
		if err != nil {
			return nil, newStatusError(http.StatusBadRequest, err.Error())
		}
		*field = id
	}
	rows, err := s.audit.ListDNS(ctx, sandboxID, query)
	if err != nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "read the pool proxy's audit: "+err.Error())
	}
	out := &workerapimodel.PoolDNSAuditResponse{Queries: make([]workerapimodel.PoolDNSAuditQuery, 0, len(rows))}
	for _, row := range rows {
		query := workerapimodel.PoolDNSAuditQuery{
			ID:             row.ID.String(),
			CreatedAt:      row.CreatedAt,
			SandboxId:      row.ClientID,
			Name:           row.Name,
			Type:           row.Type,
			Rcode:          row.RCode,
			Answers:        splitAuditList(row.Answers),
			DurationMillis: workerapi.NewOptInt64(row.DurationMillis),
		}
		if row.Error != "" {
			query.Error = workerapi.NewOptString(row.Error)
		}
		out.Queries = append(out.Queries, query)
	}
	return out, nil
}

// PoolGetHTTPAudit relays one audited exchange in full (ADR 0130 §5): every
// field the proxy's recorder wrote, rather than the summary a list returns. It
// is narrowed exactly as the list is, and a row outside the scope is not found
// rather than refused.
func (s *sandboxService) PoolGetHTTPAudit(ctx context.Context, params workerapi.PoolGetHTTPAuditParams) (*workerapimodel.PoolHTTPAuditExchangeDetail, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	if s.audit == nil {
		return nil, newStatusError(http.StatusServiceUnavailable, "the pool proxy's audit is not configured")
	}
	id, err := auditid.ParseExchange(params.ExchangeId)
	if err != nil {
		return nil, newStatusError(http.StatusBadRequest, err.Error())
	}
	sandboxID, err := auditSandbox(ctx, params.SandboxId.Or(""))
	if err != nil {
		return nil, err
	}
	row, err := s.audit.GetHTTP(ctx, sandboxID, id)
	switch {
	case errors.Is(err, proxy.ErrAuditArtifactNotFound):
		return nil, newStatusError(http.StatusNotFound, fmt.Sprintf("no audited exchange %s", params.ExchangeId))
	case err != nil:
		return nil, newStatusError(http.StatusServiceUnavailable, "read the pool proxy's audit: "+err.Error())
	}
	detail := poolHTTPAuditExchangeDetail(*row)
	return &detail, nil
}

// poolHTTPAuditExchangeDetail is the whole recorded row. Spool file names are
// deliberately not carried: they are paths on the pool's disk, and what a
// caller can do about them is read them through the artifact route, which the
// recorded flags say is possible.
func poolHTTPAuditExchangeDetail(row proxy.AuditHTTPExchange) workerapimodel.PoolHTTPAuditExchangeDetail {
	detail := workerapimodel.PoolHTTPAuditExchangeDetail{
		ID:                   row.ID.String(),
		CreatedAt:            row.CreatedAt,
		SandboxId:            row.ClientID,
		Method:               row.Method,
		URL:                  row.URL,
		Host:                 row.Host,
		Status:               row.Status,
		Blocked:              row.Blocked,
		SwappedUseIds:        splitAuditList(row.SwappedUseIDs),
		AppliedHeaders:       splitAuditList(row.AppliedHeaders),
		RequestHeaders:       unmarshalAuditHeaders(row.RequestHeaders),
		ResponseHeaders:      unmarshalAuditHeaders(row.ResponseHeaders),
		DurationMillis:       workerapi.NewOptInt64(row.DurationMillis),
		EnqueuedAt:           workerapi.NewOptDateTime(row.EnqueuedAt),
		WrittenAt:            workerapi.NewOptDateTime(row.WrittenAt),
		CacheHit:             workerapi.NewOptBool(row.CacheHit),
		CacheStored:          workerapi.NewOptBool(row.CacheStored),
		RequestBodyBytes:     workerapi.NewOptInt64(row.RequestBodyBytes),
		ResponseBytes:        workerapi.NewOptInt64(row.ResponseBytes),
		RequestBodyRecorded:  workerapi.NewOptBool(row.RequestBodyFile != ""),
		ResponseBodyRecorded: workerapi.NewOptBool(row.ResponseBodyFile != ""),
		StreamRecorded:       workerapi.NewOptBool(row.StreamFile != ""),
		Upgrade:              workerapi.NewOptBool(row.Upgrade),
		UpgradeC2sBytes:      workerapi.NewOptInt64(row.UpgradeC2SBytes),
		UpgradeS2cBytes:      workerapi.NewOptInt64(row.UpgradeS2CBytes),
		StreamDroppedChunks:  workerapi.NewOptInt64(int64(row.StreamDroppedChunks)),
		StreamDroppedBytes:   workerapi.NewOptInt64(int64(row.StreamDroppedBytes)),
	}
	for value, field := range map[string]*workerapi.OptString{
		row.BlockedReason:      &detail.BlockedReason,
		row.CacheKey:           &detail.CacheKey,
		row.CacheError:         &detail.CacheError,
		row.AppliedRuleID:      &detail.AppliedRuleId,
		row.AppliedPattern:     &detail.AppliedPattern,
		row.RequestBodyFormat:  &detail.RequestBodyFormat,
		row.RequestBodyError:   &detail.RequestBodyError,
		row.ResponseBodyFormat: &detail.ResponseBodyFormat,
		row.ResponseBodyError:  &detail.ResponseBodyError,
		row.UpgradeType:        &detail.UpgradeType,
		row.StreamSessionID:    &detail.StreamSessionId,
		row.StreamFormat:       &detail.StreamFormat,
	} {
		if value != "" {
			*field = workerapi.NewOptString(value)
		}
	}
	return detail
}

// splitAuditList reads one of the comma-joined lists the recorder stores.
func splitAuditList(stored string) []string {
	if stored == "" {
		return []string{}
	}
	return strings.Split(stored, ",")
}

// unmarshalAuditHeaders reads the headers the recorder stored as JSON. They are
// redacted where it wrote them, so what comes back is already safe to show; a
// value that does not parse is reported as no headers rather than failing the
// read, since the rest of the row is still worth having.
func unmarshalAuditHeaders(stored string) map[string][]string {
	headers := map[string][]string{}
	if strings.TrimSpace(stored) == "" {
		return headers
	}
	if err := json.Unmarshal([]byte(stored), &headers); err != nil {
		return map[string][]string{}
	}
	return headers
}

// auditSandbox is the sandbox an audit read is narrowed to: the token's, when
// the control plane scoped it to one, refusing a query that names another.
func auditSandbox(ctx context.Context, requested string) (string, error) {
	if claims, ok := SignedTokenClaimsFromContext(ctx); ok && claims.SandboxID != "" {
		if requested != "" && requested != claims.SandboxID {
			return "", newStatusError(http.StatusForbidden, "token is scoped to another sandbox")
		}
		return claims.SandboxID, nil
	}
	return requested, nil
}

// registerAuditRoutes wires the routes the generated API cannot carry: a
// recorded body or upgraded stream is an opaque stream of unbounded length.
func registerAuditRoutes(router chi.Router, service *sandboxService) {
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/audit/http/{exchangeId}/{artifact}", service.httpAuditArtifactHandler())
}

// httpAuditArtifactHandler streams one body or upgraded stream recorded beside
// an audit row, relayed from the proxy's control API. It is authorized like the
// list — audit:read, narrowed to the token's sandbox — and a row belonging to
// another sandbox reads as not found rather than forbidden, so the answer says
// nothing about rows outside the scope.
func (s *sandboxService) httpAuditArtifactHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeTree(r, ScopeAuditRead); err != nil {
			writeProblem(w, statusCodeForTreeError(err), err.Error())
			return
		}
		if s.audit == nil {
			writeProblem(w, http.StatusServiceUnavailable, "the pool proxy's audit is not configured")
			return
		}
		id, err := auditid.ParseExchange(chi.URLParam(r, "exchangeId"))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		sandboxID, err := auditSandbox(r.Context(), r.URL.Query().Get("sandboxId"))
		if err != nil {
			writeProblem(w, statusCodeForTreeError(err), err.Error())
			return
		}
		artifact, err := s.audit.OpenHTTPArtifact(r.Context(), sandboxID, id, chi.URLParam(r, "artifact"))
		switch {
		case errors.Is(err, proxy.ErrAuditArtifactNotFound):
			// Not http.NotFound: that is the router's text/plain 404, which is
			// what an agent too old for this route answers, and the two must
			// not read alike.
			writeProblem(w, http.StatusNotFound, "the pool proxy recorded no "+chi.URLParam(r, "artifact")+" for exchange "+id.String())
			return
		case err != nil:
			writeProblem(w, http.StatusServiceUnavailable, "read the pool proxy's audit: "+err.Error())
			return
		}
		defer artifact.Body.Close()
		w.Header().Set("Content-Type", artifact.ContentType)
		w.Header().Set(AuditArtifactFormatHeader, artifact.Format)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, artifact.Body)
	})
}

// AuditArtifactFormatHeader carries the spool format of a relayed artifact: raw
// bytes for a body, framed for an upgraded stream.
const AuditArtifactFormatHeader = "X-Discobox-Audit-Format"

func poolHTTPAuditExchange(row proxy.AuditHTTPExchange) workerapimodel.PoolHTTPAuditExchange {
	uses := []string{}
	if row.SwappedUseIDs != "" {
		uses = strings.Split(row.SwappedUseIDs, ",")
	}
	exchange := workerapimodel.PoolHTTPAuditExchange{
		ID:               row.ID.String(),
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
