package handlers

import (
	"context"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

// httpAuditPools answers ListHTTPAudit and records the filter; every other
// PoolService method is the nil embedded interface and panics if reached.
type httpAuditPools struct {
	svcapi.PoolService
	filter *svcapi.HTTPAuditFilter
	result *svcapi.HTTPAuditResult
}

func (p httpAuditPools) ListHTTPAudit(_ context.Context, _ string, filter svcapi.HTTPAuditFilter) (*svcapi.HTTPAuditResult, error) {
	*p.filter = filter
	return p.result, nil
}

// The response is built by round-tripping the service result through JSON, and
// the exchange embeds the runtime's type beside its pool: every field has to
// come out flat, and both lists have to be lists.
func TestListHTTPAuditReturnsEveryFieldAndBothLists(t *testing.T) {
	createdAt := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	var filter svcapi.HTTPAuditFilter
	h := New(svcapi.Services{Pools: httpAuditPools{filter: &filter, result: &svcapi.HTTPAuditResult{
		Exchanges: []svcapi.PoolHTTPAuditExchange{{PoolID: "pool-a", HTTPAuditExchange: sandbox.HTTPAuditExchange{
			ID: 7, CreatedAt: createdAt, SandboxID: "sbx_gone", Method: "POST", URL: "https://api.github.com/repos/o/r/pulls",
			Host: "api.github.com", Status: 201, DurationMillis: 340, SwappedUseIDs: []string{"use_x"},
			RequestBodyBytes: 90, ResponseBytes: 2048,
		}}},
		UnavailablePools: []svcapi.UnavailableAuditPool{{PoolID: "pool-c", Reason: "its pool agent predates the audit read"}},
	}}})

	res, err := h.ListHTTPAudit(context.Background(), serverapi.ListHTTPAuditParams{
		ProjectId: "project-1", SandboxId: serverapi.NewOptString("sbx_gone"), UseId: serverapi.NewOptString("use_x"),
	})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if filter.SandboxID != "sbx_gone" || filter.UseID != "use_x" || filter.Limit != 100 {
		t.Fatalf("filter = %+v, want the request's with the default limit", filter)
	}
	body, ok := res.(*serverapi.ListHTTPAuditBody)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	if len(body.Exchanges) != 1 || len(body.UnavailablePools) != 1 {
		t.Fatalf("body = %+v", body)
	}
	e := body.Exchanges[0]
	if e.PoolId != "pool-a" || e.ID != 7 || !e.CreatedAt.Equal(createdAt) || e.SandboxId != "sbx_gone" || e.Method != "POST" ||
		e.Host != "api.github.com" || e.Status != 201 || e.DurationMillis.Or(0) != 340 || len(e.SwappedUseIds) != 1 ||
		e.SwappedUseIds[0] != "use_x" || e.RequestBodyBytes.Or(0) != 90 || e.ResponseBytes.Or(0) != 2048 {
		t.Fatalf("exchange lost a field on the way out: %+v", e)
	}
	if body.UnavailablePools[0].PoolId != "pool-c" || body.UnavailablePools[0].Reason == "" {
		t.Fatalf("unavailable pool = %+v", body.UnavailablePools[0])
	}
}
