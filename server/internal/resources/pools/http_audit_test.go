package pools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// auditPoolProvider answers each pool's audit read from a table, and records
// which pools were asked and with what.
type auditPoolProvider struct {
	stubPoolProvider
	mu      sync.Mutex
	rows    map[string][]sandbox.HTTPAuditExchange
	errs    map[string]error
	hang    map[string]bool
	queries map[string]sandbox.HTTPAuditQuery

	dnsRows    map[string][]sandbox.DNSAuditQuery
	dnsFilters map[string]sandbox.DNSAuditFilter
}

func (p *auditPoolProvider) ListDNSAudit(_ context.Context, pool *model.Pool, filter sandbox.DNSAuditFilter) ([]sandbox.DNSAuditQuery, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dnsFilters[pool.ID] = filter
	if err := p.errs[pool.ID]; err != nil {
		return nil, err
	}
	return p.dnsRows[pool.ID], nil
}

func (p *auditPoolProvider) ListHTTPAudit(ctx context.Context, pool *model.Pool, query sandbox.HTTPAuditQuery) ([]sandbox.HTTPAuditExchange, error) {
	p.mu.Lock()
	p.queries[pool.ID] = query
	hang := p.hang[pool.ID]
	p.mu.Unlock()
	if hang {
		// A host that accepts the connection and never answers.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[pool.ID]; err != nil {
		return nil, err
	}
	return p.rows[pool.ID], nil
}

func (p *auditPoolProvider) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id := range p.queries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// newAuditService is a project with three pools: two answer, one runs an agent
// too old for the read. sandbox-live runs on pool-b; sandbox-gone has no row.
func newAuditService(t *testing.T) (*Service, *auditPoolProvider) {
	return newAuditServiceWith(t, nil)
}

// newAuditServiceWith lets a test shape each pool before it is stored, for
// lifecycle fields the store only writes on create.
func newAuditServiceWith(t *testing.T, shape func(*model.Pool)) (*Service, *auditPoolProvider) {
	t.Helper()
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	at := func(minutesAgo int) time.Time { return time.Now().UTC().Add(-time.Duration(minutesAgo) * time.Minute) }
	provider := &auditPoolProvider{
		rows: map[string][]sandbox.HTTPAuditExchange{
			"pool-a": {
				{ID: 2, CreatedAt: at(1), SandboxID: "sandbox-gone", Host: "a.example.com", SwappedUseIDs: []string{}},
				{ID: 1, CreatedAt: at(5), SandboxID: "sandbox-gone", Host: "a.example.com", SwappedUseIDs: []string{}},
			},
			"pool-b": {
				{ID: 9, CreatedAt: at(3), SandboxID: "sandbox-live", Host: "b.example.com", SwappedUseIDs: []string{"use_x"}},
			},
		},
		errs:    map[string]error{"pool-c": sandbox.ErrPoolAgentUnsupported},
		hang:    map[string]bool{},
		queries: map[string]sandbox.HTTPAuditQuery{},
		dnsRows: map[string][]sandbox.DNSAuditQuery{
			"pool-a": {{ID: 4, CreatedAt: at(2), SandboxID: "sandbox-gone", Name: "a.example.com", Answers: []string{}}},
			"pool-b": {
				{ID: 7, CreatedAt: at(1), SandboxID: "sandbox-live", Name: "b.example.com", Answers: []string{"192.0.2.1"}},
				{ID: 6, CreatedAt: at(4), SandboxID: "sandbox-live", Name: "b.example.com", Answers: []string{"192.0.2.1"}},
			},
		},
		dnsFilters: map[string]sandbox.DNSAuditFilter{},
	}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("audit", provider)
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "audit", Name: "audit"}
	if err := appStore.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	for _, id := range []string{"pool-a", "pool-b", "pool-c"} {
		pool := &model.Pool{ID: id, ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: id, ProviderInstanceID: instance.ID}, RegisteredAt: &registeredAt}
		pool.DesiredState = model.DesiredStatePresent
		if shape != nil {
			shape(pool)
		}
		if err := appStore.CreatePool(ctx, pool); err != nil {
			t.Fatalf("create pool %s: %v", id, err)
		}
	}
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sandbox-live", ProjectID: "project-1", PoolID: "pool-b", CreatedByUserID: "user-1", Name: "sandbox-live",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return NewService(appStore, manager, NewControlPlane(appStore, nil)), provider
}

// The whole project, merged newest first across pools, with the pool that did
// not answer named rather than dropped.
func TestListHTTPAuditMergesPoolsAndNamesTheOneThatDidNotAnswer(t *testing.T) {
	svc, provider := newAuditService(t)
	result, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if got, want := provider.asked(), []string{"pool-a", "pool-b", "pool-c"}; !slices.Equal(got, want) {
		t.Fatalf("asked %v, want every pool %v", got, want)
	}
	var order []string
	for _, e := range result.Exchanges {
		order = append(order, e.PoolID)
	}
	if want := []string{"pool-a", "pool-b", "pool-a"}; !slices.Equal(order, want) {
		t.Fatalf("merged order by pool = %v, want newest first across pools %v", order, want)
	}
	if len(result.UnavailablePools) != 1 || result.UnavailablePools[0].PoolID != "pool-c" ||
		!strings.Contains(result.UnavailablePools[0].Reason, "predates") {
		t.Fatalf("unavailable = %+v, want pool-c named as running an older agent", result.UnavailablePools)
	}
}

// A follower reads forward from its cursor: the merge is oldest first and keeps
// the oldest N, and every filter reaches every pool.
func TestListHTTPAuditReadsForwardAndPassesFiltersToEachPool(t *testing.T) {
	svc, provider := newAuditService(t)
	for id, rows := range provider.rows {
		slices.Reverse(rows)
		provider.rows[id] = rows
	}
	blocked := true
	result, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{
		Limit: 2, Ascending: true, MinStatus: 400, MaxStatus: 499, Blocked: &blocked,
	})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	for _, id := range []string{"pool-a", "pool-b"} {
		q := provider.queries[id]
		if !q.Ascending || q.MinStatus != 400 || q.MaxStatus != 499 || q.Blocked == nil || !*q.Blocked {
			t.Fatalf("%s asked %+v, want the whole filter", id, q)
		}
	}
	if len(result.Exchanges) != 2 || result.Exchanges[0].ID != 1 || result.Exchanges[1].ID != 9 {
		t.Fatalf("exchanges = %+v, want the two oldest across pools, oldest first", result.Exchanges)
	}
}

// The newest N across pools can all come from one pool, so every pool is asked
// for the whole limit and the merge is what truncates.
func TestListHTTPAuditAsksEachPoolForTheWholeLimit(t *testing.T) {
	svc, provider := newAuditService(t)
	result, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	for _, id := range []string{"pool-a", "pool-b"} {
		if provider.queries[id].Limit != 2 {
			t.Fatalf("%s asked for %d, want the whole limit 2", id, provider.queries[id].Limit)
		}
	}
	if len(result.Exchanges) != 2 || result.Exchanges[0].ID != 2 || result.Exchanges[1].ID != 9 {
		t.Fatalf("exchanges = %+v, want the two newest across pools", result.Exchanges)
	}
}

func TestListHTTPAuditChoosesWhichPoolsToAsk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter services.HTTPAuditFilter
		want   []string
	}{
		// A live sandbox's exchanges are on the pool it runs on.
		{name: "live sandbox", filter: services.HTTPAuditFilter{SandboxID: "sandbox-live"}, want: []string{"pool-b"}},
		// A purged one's could be on any: nothing left says which.
		{name: "purged sandbox", filter: services.HTTPAuditFilter{SandboxID: "sandbox-gone"}, want: []string{"pool-a", "pool-b", "pool-c"}},
		{name: "one pool", filter: services.HTTPAuditFilter{PoolID: "pool-a"}, want: []string{"pool-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider := newAuditService(t)
			if _, err := svc.ListHTTPAudit(context.Background(), "project-1", tc.filter); err != nil {
				t.Fatalf("ListHTTPAudit() error = %v", err)
			}
			if got := provider.asked(); !slices.Equal(got, tc.want) {
				t.Fatalf("asked %v, want %v", got, tc.want)
			}
			for _, id := range tc.want {
				if provider.queries[id].SandboxID != tc.filter.SandboxID {
					t.Fatalf("%s asked for sandbox %q, want %q", id, provider.queries[id].SandboxID, tc.filter.SandboxID)
				}
			}
		})
	}
}

func TestListHTTPAuditUnknownPoolIsNotFound(t *testing.T) {
	svc, _ := newAuditService(t)
	_, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{PoolID: "pool-z"})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusNotFound {
		t.Fatalf("ListHTTPAudit() for an unknown pool = %v, want 404", err)
	}
}

// One host that never answers must not hold the read: it gets its deadline,
// is reported, and every other pool's answer still arrives.
func TestListHTTPAuditBoundsAPoolThatNeverAnswers(t *testing.T) {
	previous := auditPoolReadTimeout
	auditPoolReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { auditPoolReadTimeout = previous })

	svc, provider := newAuditService(t)
	provider.hang["pool-b"] = true
	started := time.Now()
	result, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("read took %s; a hung pool held it past its deadline", elapsed)
	}
	var hung *services.UnavailableAuditPool
	for i := range result.UnavailablePools {
		if result.UnavailablePools[i].PoolID == "pool-b" {
			hung = &result.UnavailablePools[i]
		}
	}
	if hung == nil || !strings.Contains(hung.Reason, "did not answer") {
		t.Fatalf("unavailable = %+v, want pool-b reported as not answering", result.UnavailablePools)
	}
	if len(result.Exchanges) != 2 {
		t.Fatalf("exchanges = %+v, want pool-a's two despite pool-b hanging", result.Exchanges)
	}
}

// A pool being deleted, or one whose agent never registered, has nothing to
// answer. It is reported with why, and not asked.
func TestListHTTPAuditReportsPoolsWithNoAgentWithoutAskingThem(t *testing.T) {
	svc, provider := newAuditServiceWith(t, func(pool *model.Pool) {
		switch pool.ID {
		case "pool-a":
			pool.DesiredState = model.DesiredStateDeleted
		case "pool-b":
			pool.RegisteredAt = nil
		}
	})
	ctx := context.Background()

	result, err := svc.ListHTTPAudit(ctx, "project-1", services.HTTPAuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if got := provider.asked(); !slices.Equal(got, []string{"pool-c"}) {
		t.Fatalf("asked %v, want only pool-c, the one pool with an agent", got)
	}
	reasons := map[string]string{}
	for _, u := range result.UnavailablePools {
		reasons[u.PoolID] = u.Reason
	}
	if !strings.Contains(reasons["pool-a"], "deleted") || !strings.Contains(reasons["pool-b"], "registered") {
		t.Fatalf("unavailable = %+v, want pool-a as being deleted and pool-b as unregistered", result.UnavailablePools)
	}
}

func (p *auditPoolProvider) OpenHTTPAuditArtifact(_ context.Context, pool *model.Pool, sandboxID string, id auditid.ExchangeID, artifact string) (*sandbox.HTTPAuditArtifact, error) {
	if pool.ID == "pool-a" && id == 2 && artifact == "response-body" && (sandboxID == "" || sandboxID == "sandbox-gone") {
		return &sandbox.HTTPAuditArtifact{Body: io.NopCloser(strings.NewReader("body")), Format: "raw"}, nil
	}
	return nil, sandbox.ErrNotFound
}

// A body is read from the one pool that recorded it; missing is a 404 that says
// what is missing, and a pool with no agent is a 503 rather than a wait.
func TestOpenHTTPAuditArtifactReadsTheNamedPool(t *testing.T) {
	svc, _ := newAuditServiceWith(t, func(pool *model.Pool) {
		if pool.ID == "pool-b" {
			pool.RegisteredAt = nil
		}
	})
	ctx := context.Background()
	opened, err := svc.OpenHTTPAuditArtifact(ctx, "project-1", "pool-a", "sandbox-gone", 2, "response-body")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = opened.Body.Close()

	_, err = svc.OpenHTTPAuditArtifact(ctx, "project-1", "pool-a", "sandbox-other", 2, "response-body")
	if status := statusOf(err); status != http.StatusNotFound || !strings.Contains(err.Error(), "exchange http_2") {
		t.Fatalf("open another sandbox's body = %v (status %d), want 404 naming the exchange", err, status)
	}
	_, err = svc.OpenHTTPAuditArtifact(ctx, "project-1", "pool-b", "", 9, "response-body")
	if status := statusOf(err); status != http.StatusServiceUnavailable {
		t.Fatalf("open from an unregistered pool = %v (status %d), want 503", err, status)
	}
	_, err = svc.OpenHTTPAuditArtifact(ctx, "project-1", "pool-z", "", 1, "response-body")
	if status := statusOf(err); status != http.StatusNotFound {
		t.Fatalf("open from a pool that does not exist = %v (status %d), want 404", err, status)
	}
}

func statusOf(err error) int {
	var status apperrors.StatusError
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

// Each pool reads from its own cursor, and a pool the caller has no cursor for
// still reads by time — that is how a pool whose rows have not been seen yet
// joins a follow already running.
func TestListHTTPAuditGivesEachPoolItsOwnCursor(t *testing.T) {
	svc, provider := newAuditService(t)
	since := time.Now().UTC().Add(-time.Hour)
	if _, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{
		Limit: 10, Ascending: true, Since: since, After: map[string]auditid.ExchangeID{"pool-a": 42},
	}); err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if got := provider.queries["pool-a"]; got.AfterID != 42 {
		t.Fatalf("pool-a asked %+v, want its cursor", got)
	}
	if got := provider.queries["pool-b"]; got.AfterID != 0 || !got.Since.Equal(since) {
		t.Fatalf("pool-b asked %+v, want the time bound and no cursor", got)
	}
}

// The limit has to cut in the same order the cursor advances. A pool reading
// from a cursor answers in write order, which is not time order, so a merge
// that sorted everything by time and sliced would keep a pool's later-written
// row and drop an earlier-written one — and the caller, moving that pool's
// cursor to the highest ID it was handed, would never be offered the dropped
// row again.
func TestListHTTPAuditNeverCutsInsideAPoolsWriteOrder(t *testing.T) {
	svc, provider := newAuditService(t)
	at := func(minutesAgo int) time.Time { return time.Now().UTC().Add(-time.Duration(minutesAgo) * time.Minute) }
	// Each pool answers in write order. pool-a's row 2 was written after row 1
	// but stamped before it, which is what the recorder's queue produces.
	provider.rows = map[string][]sandbox.HTTPAuditExchange{
		"pool-a": {
			{ID: 1, CreatedAt: at(1), SandboxID: "sandbox-gone", SwappedUseIDs: []string{}},
			{ID: 2, CreatedAt: at(9), SandboxID: "sandbox-gone", SwappedUseIDs: []string{}},
		},
		"pool-b": {
			{ID: 10, CreatedAt: at(8), SandboxID: "sandbox-live", SwappedUseIDs: []string{}},
			{ID: 11, CreatedAt: at(7), SandboxID: "sandbox-live", SwappedUseIDs: []string{}},
		},
	}
	result, err := svc.ListHTTPAudit(context.Background(), "project-1", services.HTTPAuditFilter{
		Limit: 2, Ascending: true, After: map[string]auditid.ExchangeID{"pool-a": 0, "pool-b": 9},
	})
	if err != nil {
		t.Fatalf("ListHTTPAudit() error = %v", err)
	}
	if len(result.Exchanges) != 2 {
		t.Fatalf("exchanges = %+v, want the limit", result.Exchanges)
	}
	// Whatever survives the cut has to be a prefix of its pool's page: nothing
	// kept may have an unread record behind it.
	seen := map[string]auditid.ExchangeID{}
	for _, exchange := range result.Exchanges {
		page := provider.rows[exchange.PoolID]
		for _, row := range page {
			if row.ID < exchange.ID && !slices.ContainsFunc(result.Exchanges, func(kept services.PoolHTTPAuditExchange) bool {
				return kept.PoolID == exchange.PoolID && kept.ID == row.ID
			}) {
				t.Fatalf("kept %s/%s but dropped %s, which its pool wrote earlier: the cursor would skip it",
					exchange.PoolID, exchange.ID, row.ID)
			}
		}
		seen[exchange.PoolID] = exchange.ID
	}
}

// The DNS trail is read and merged exactly as the HTTP one: every pool, newest
// first, the pool that could not answer named, and each pool's own cursor.
func TestListDNSAuditMergesPoolsWithTheirOwnCursors(t *testing.T) {
	svc, provider := newAuditService(t)
	result, err := svc.ListDNSAudit(context.Background(), "project-1", services.DNSAuditFilter{
		Name:  "b.example.com",
		After: map[string]auditid.DNSQueryID{"pool-b": 5},
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListDNSAudit() error = %v", err)
	}
	var got []string
	for _, q := range result.Queries {
		got = append(got, q.PoolID+"/"+q.ID.String())
	}
	if want := []string{"pool-b/dns_7", "pool-a/dns_4", "pool-b/dns_6"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
	if len(result.UnavailablePools) != 1 || result.UnavailablePools[0].PoolID != "pool-c" || !strings.Contains(result.UnavailablePools[0].Reason, "predates") {
		t.Fatalf("unavailable = %+v, want pool-c named", result.UnavailablePools)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if f := provider.dnsFilters["pool-b"]; f.AfterID != 5 || f.Name != "b.example.com" {
		t.Fatalf("pool-b filter = %+v, want its own cursor and the name", f)
	}
	if f := provider.dnsFilters["pool-a"]; f.AfterID != 0 {
		t.Fatalf("pool-a filter = %+v, want no cursor", f)
	}
}

// A sandbox that still exists narrows the read to its pool.
func TestListDNSAuditAsksOnlyTheLiveSandboxesPool(t *testing.T) {
	svc, provider := newAuditService(t)
	if _, err := svc.ListDNSAudit(context.Background(), "project-1", services.DNSAuditFilter{SandboxID: "sandbox-live"}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.dnsFilters) != 1 || provider.dnsFilters["pool-b"].SandboxID != "sandbox-live" {
		t.Fatalf("asked %v, want only pool-b for sandbox-live", provider.dnsFilters)
	}
}
