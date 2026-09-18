package handlers

import (
	"context"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/auditid"
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
	if e.PoolId != "pool-a" || e.ID != "http_7" || !e.CreatedAt.Equal(createdAt) || e.SandboxId != "sbx_gone" || e.Method != "POST" ||
		e.Host != "api.github.com" || e.Status != 201 || e.DurationMillis.Or(0) != 340 || len(e.SwappedUseIds) != 1 ||
		e.SwappedUseIds[0] != "use_x" || e.RequestBodyBytes.Or(0) != 90 || e.ResponseBytes.Or(0) != 2048 {
		t.Fatalf("exchange lost a field on the way out: %+v", e)
	}
	if body.UnavailablePools[0].PoolId != "pool-c" || body.UnavailablePools[0].Reason == "" {
		t.Fatalf("unavailable pool = %+v", body.UnavailablePools[0])
	}
	if filter.Blocked != nil || filter.Ascending || filter.MinStatus != 0 {
		t.Fatalf("filter = %+v, want no blocked, status or order filter when none was asked for", filter)
	}
}

// blocked is tri-state, and the status bounds and order reach the service.
func TestListHTTPAuditPassesStatusBlockedAndOrder(t *testing.T) {
	var filter svcapi.HTTPAuditFilter
	h := New(svcapi.Services{Pools: httpAuditPools{filter: &filter, result: &svcapi.HTTPAuditResult{Exchanges: []svcapi.PoolHTTPAuditExchange{}, UnavailablePools: []svcapi.UnavailableAuditPool{}}}})
	_, err := h.ListHTTPAudit(context.Background(), serverapi.ListHTTPAuditParams{
		ProjectId: "project-1", MinStatus: serverapi.NewOptInt(500), MaxStatus: serverapi.NewOptInt(599),
		Blocked: serverapi.NewOptBool(false), Order: serverapi.NewOptListHTTPAuditOrder(serverapi.ListHTTPAuditOrderAsc),
	})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if filter.MinStatus != 500 || filter.MaxStatus != 599 || filter.Blocked == nil || *filter.Blocked || !filter.Ascending {
		t.Fatalf("filter = %+v", filter)
	}
}

// A row ID means something only on the pool that issued it, so the cursor is
// per pool. A malformed one is refused rather than dropped: silently ignoring
// it would restart the follow from the time bound and re-print rows.
func TestListHTTPAuditReadsAPerPoolCursor(t *testing.T) {
	var filter svcapi.HTTPAuditFilter
	h := New(svcapi.Services{Pools: httpAuditPools{filter: &filter, result: &svcapi.HTTPAuditResult{
		Exchanges: []svcapi.PoolHTTPAuditExchange{}, UnavailablePools: []svcapi.UnavailableAuditPool{},
	}}})
	res, err := h.ListHTTPAudit(context.Background(), serverapi.ListHTTPAuditParams{
		ProjectId: "project-1", After: []string{"pool-a:http_7", "pool-b:http_2", "pool-a:http_9"},
	})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if _, ok := res.(*serverapi.ListHTTPAuditBody); !ok {
		t.Fatalf("response = %T", res)
	}
	// The highest id for a repeated pool wins, so a row cannot be read twice.
	if len(filter.After) != 2 || filter.After["pool-a"] != 9 || filter.After["pool-b"] != 2 {
		t.Fatalf("After = %v", filter.After)
	}

	// A bare row number is the audit database's own spelling, not the API's.
	for _, bad := range []string{"pool-a", "pool-a:", ":http_7", "pool-a:http_0", "pool-a:7", "pool-a:x", "pool-a:http_x"} {
		res, err := h.ListHTTPAudit(context.Background(), serverapi.ListHTTPAuditParams{
			ProjectId: "project-1", After: []string{bad},
		})
		if err != nil {
			t.Fatalf("ListHTTPAudit(%q) error = %v", bad, err)
		}
		problem, ok := res.(*serverapi.ErrorModelStatusCode)
		if !ok || problem.StatusCode != 400 {
			t.Fatalf("after %q = %T %+v, want 400", bad, res, res)
		}
	}
}

func (p httpAuditPools) GetHTTPAudit(_ context.Context, _, poolID, _ string, id auditid.ExchangeID) (*svcapi.PoolHTTPAuditExchangeDetail, error) {
	return &svcapi.PoolHTTPAuditExchangeDetail{
		PoolID: poolID,
		HTTPAuditExchangeDetail: sandbox.HTTPAuditExchangeDetail{
			HTTPAuditExchange: sandbox.HTTPAuditExchange{
				ID: id, CreatedAt: time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC), SandboxID: "sbx_1",
				Method: "GET", URL: "https://api.github.com/", Host: "api.github.com", Status: 200,
				SwappedUseIDs: []string{},
			},
			WrittenAt:       time.Date(2026, 9, 17, 10, 0, 2, 0, time.UTC),
			RequestHeaders:  map[string][]string{"Authorization": {"[REDACTED]"}},
			ResponseHeaders: map[string][]string{},
			AppliedHeaders:  []string{},
		},
	}, nil
}

// The body is built by round-tripping the service's record through JSON, and
// the pool is not part of the record — so a detail that did not carry it beside
// the record failed the required field and answered 500 for every read.
func TestGetHTTPAuditCarriesThePoolAndTheWholeRecord(t *testing.T) {
	h := New(svcapi.Services{Pools: httpAuditPools{}})
	res, err := h.GetHTTPAudit(context.Background(), serverapi.GetHTTPAuditParams{
		ProjectId: "project-1", PoolId: "pool-a", ExchangeId: "http_7",
	})
	if err != nil {
		t.Fatalf("GetHTTPAudit() error = %v", err)
	}
	detail, ok := res.(*serverapi.HTTPAuditExchangeDetail)
	if !ok {
		t.Fatalf("response = %T %+v", res, res)
	}
	if detail.PoolId != "pool-a" || detail.ID != "http_7" || !detail.WrittenAt.Or(time.Time{}).Equal(time.Date(2026, 9, 17, 10, 0, 2, 0, time.UTC)) {
		t.Fatalf("detail = %+v, want the record with its pool", detail)
	}
	if got := detail.RequestHeaders["Authorization"]; len(got) != 1 || got[0] != "[REDACTED]" {
		t.Fatalf("headers = %v, want what the recorder stored", detail.RequestHeaders)
	}

	// An ID the audit database never issues is refused before any pool is read.
	res, err = h.GetHTTPAudit(context.Background(), serverapi.GetHTTPAuditParams{
		ProjectId: "project-1", PoolId: "pool-a", ExchangeId: "7",
	})
	if err != nil {
		t.Fatalf("GetHTTPAudit() error = %v", err)
	}
	if problem, ok := res.(*serverapi.ErrorModelStatusCode); !ok || problem.StatusCode != 400 {
		t.Fatalf("a bare row number = %T %+v, want 400", res, res)
	}
}
