package handlers

import (
	"context"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

// dnsAuditPools answers ListDNSAudit and records the filter; every other
// PoolService method is the nil embedded interface and panics if reached.
type dnsAuditPools struct {
	svcapi.PoolService
	filter *svcapi.DNSAuditFilter
	result *svcapi.DNSAuditResult
}

func (p dnsAuditPools) ListDNSAudit(_ context.Context, _ string, filter svcapi.DNSAuditFilter) (*svcapi.DNSAuditResult, error) {
	*p.filter = filter
	return p.result, nil
}

// The query embeds the runtime's type beside its pool, so every field has to
// come out flat through the JSON round trip, and both lists have to be lists.
func TestListDNSAuditReturnsEveryFieldAndReadsPoolCursors(t *testing.T) {
	createdAt := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	var filter svcapi.DNSAuditFilter
	h := New(svcapi.Services{Pools: dnsAuditPools{filter: &filter, result: &svcapi.DNSAuditResult{
		Queries: []svcapi.PoolDNSAuditQuery{{PoolID: "pool-a", DNSAuditQuery: sandbox.DNSAuditQuery{
			ID: 7, CreatedAt: createdAt, SandboxID: "sbx_1", Name: "api.github.com", Type: "A", RCode: "NOERROR",
			Answers: []string{"192.0.2.1"}, DurationMillis: 3,
		}}},
		UnavailablePools: []svcapi.UnavailableAuditPool{{PoolID: "pool-c", Reason: "it did not answer"}},
	}}})
	res, err := h.ListDNSAudit(context.Background(), serverapi.ListDNSAuditParams{
		ProjectId: "project-1", Name: serverapi.NewOptString("api.github.com"),
		Order: serverapi.NewOptListDNSAuditOrder(serverapi.ListDNSAuditOrderAsc),
		After: []string{"pool-a:dns_4", "pool-a:dns_6"},
	})
	if err != nil {
		t.Fatalf("ListDNSAudit() error = %v", err)
	}
	if filter.Name != "api.github.com" || !filter.Ascending || filter.Limit != 100 || filter.After["pool-a"] != 6 {
		t.Fatalf("filter = %+v", filter)
	}
	body, ok := res.(*serverapi.ListDNSAuditBody)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	if len(body.Queries) != 1 || len(body.UnavailablePools) != 1 {
		t.Fatalf("body = %+v", body)
	}
	q := body.Queries[0]
	if q.PoolId != "pool-a" || q.ID != "dns_7" || !q.CreatedAt.Equal(createdAt) || q.Name != "api.github.com" || q.Rcode != "NOERROR" ||
		len(q.Answers) != 1 || q.DurationMillis.Or(0) != 3 {
		t.Fatalf("query = %+v", q)
	}

	// Another trail's cursor is refused, not read as a row number here.
	res, err = h.ListDNSAudit(context.Background(), serverapi.ListDNSAuditParams{ProjectId: "project-1", After: []string{"pool-a:http_7"}})
	if err != nil {
		t.Fatal(err)
	}
	if problem, ok := res.(*serverapi.ErrorModelStatusCode); !ok || problem.StatusCode != 400 {
		t.Fatalf("an HTTP cursor = %T %+v, want 400", res, res)
	}
}
