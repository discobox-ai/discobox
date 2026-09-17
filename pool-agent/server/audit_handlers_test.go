package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/proxy"
)

// recordingAuditReader records what the relay asked the proxy for and answers
// with fixed rows, or an error.
type recordingAuditReader struct {
	sandboxID string
	query     proxy.AuditQuery
	calls     int
	rows      []proxy.AuditHTTPExchange
	err       error
}

func (r *recordingAuditReader) ListHTTP(_ context.Context, sandboxID string, query proxy.AuditQuery) ([]proxy.AuditHTTPExchange, error) {
	r.calls++
	r.sandboxID, r.query = sandboxID, query
	return r.rows, r.err
}

func newAuditRouter(t *testing.T, reader AuditReader) (http.Handler, func(projectID, poolID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               sandboxruntime.NewMemorySandboxRuntime(),
		Audit:                 reader,
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router, sign
}

func auditRequest(router http.Handler, query, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/audit/http"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// The scope is what stands between any control-plane token for this pool and
// every sandbox's traffic. An operation missing from requiredPoolOperationScope
// needs no scope at all, so this is the check that would fail open.
func TestPoolListHTTPAuditRequiresAuditRead(t *testing.T) {
	reader := &recordingAuditReader{}
	router, sign := newAuditRouter(t, reader)
	resp := auditRequest(router, "", sign("project-1", "pool-1", "", ScopeSandboxRead))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}
	if reader.calls != 0 {
		t.Fatal("the proxy was read without audit:read")
	}
}

// A token the control plane scoped to one sandbox reads that sandbox, whatever
// the query says: omitted, it is filled in; naming another, it is refused.
func TestPoolListHTTPAuditNarrowsToTheTokensSandbox(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		tokenScope  string
		wantStatus  int
		wantSandbox string
	}{
		{name: "token names it, query does not", query: "", tokenScope: "sandbox-1", wantStatus: http.StatusOK, wantSandbox: "sandbox-1"},
		{name: "both name the same one", query: "?sandboxId=sandbox-1", tokenScope: "sandbox-1", wantStatus: http.StatusOK, wantSandbox: "sandbox-1"},
		{name: "query names another", query: "?sandboxId=sandbox-2", tokenScope: "sandbox-1", wantStatus: http.StatusForbidden},
		{name: "pool-wide token, query narrows", query: "?sandboxId=sandbox-2", tokenScope: "", wantStatus: http.StatusOK, wantSandbox: "sandbox-2"},
		{name: "pool-wide read", query: "", tokenScope: "", wantStatus: http.StatusOK, wantSandbox: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingAuditReader{}
			router, sign := newAuditRouter(t, reader)
			resp := auditRequest(router, tc.query, sign("project-1", "pool-1", tc.tokenScope, ScopeAuditRead))
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				if reader.calls != 0 {
					t.Fatal("the proxy was read for a refused request")
				}
				return
			}
			if reader.sandboxID != tc.wantSandbox {
				t.Fatalf("proxy read for sandbox %q, want %q", reader.sandboxID, tc.wantSandbox)
			}
		})
	}
}

func TestPoolListHTTPAuditPassesFiltersAndMapsRows(t *testing.T) {
	createdAt := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	reader := &recordingAuditReader{rows: []proxy.AuditHTTPExchange{{
		ID: 7, CreatedAt: createdAt, ClientID: "sandbox-1", Method: "GET", URL: "https://api.github.com/user",
		Host: "api.github.com", Status: 200, DurationMillis: 42, SwappedUseIDs: "use_a,use_b",
	}}}
	router, sign := newAuditRouter(t, reader)
	resp := auditRequest(router, "?host=api.github.com&useId=use_a&since=2026-09-17T09:00:00%2B09:00&limit=5",
		sign("project-1", "pool-1", "", ScopeAuditRead))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	wantSince := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	if reader.query.Host != "api.github.com" || reader.query.UseID != "use_a" || reader.query.Limit != 5 || !reader.query.Since.Equal(wantSince) {
		t.Fatalf("proxy query = %+v, want the request's filters", reader.query)
	}
	var body struct {
		Exchanges []struct {
			ID            int64     `json:"id"`
			CreatedAt     time.Time `json:"createdAt"`
			SandboxID     string    `json:"sandboxId"`
			Status        int       `json:"status"`
			SwappedUseIDs []string  `json:"swappedUseIds"`
		} `json:"exchanges"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Exchanges) != 1 {
		t.Fatalf("exchanges = %+v", body.Exchanges)
	}
	got := body.Exchanges[0]
	if got.ID != 7 || got.SandboxID != "sandbox-1" || got.Status != 200 || !got.CreatedAt.Equal(createdAt) ||
		!reflect.DeepEqual(got.SwappedUseIDs, []string{"use_a", "use_b"}) {
		t.Fatalf("exchange = %+v, want the row with its uses split", got)
	}
}

// A row with no swapped use is an empty list, not null: the response requires
// the array, and a reader should not have to tell the two apart.
func TestPoolListHTTPAuditNoUsesIsAnEmptyList(t *testing.T) {
	reader := &recordingAuditReader{rows: []proxy.AuditHTTPExchange{{ID: 1, ClientID: "sandbox-1", Method: "GET", Host: "example.com"}}}
	router, sign := newAuditRouter(t, reader)
	resp := auditRequest(router, "", sign("project-1", "pool-1", "", ScopeAuditRead))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	if !json.Valid(resp.Body.Bytes()) || !strings.Contains(resp.Body.String(), `"swappedUseIds":[]`) {
		t.Fatalf("body = %s, want an empty swappedUseIds list", resp.Body.String())
	}
}

func TestPoolListHTTPAuditReportsAnUnreachableProxy(t *testing.T) {
	router, sign := newAuditRouter(t, &recordingAuditReader{err: errors.New("connection refused")})
	resp := auditRequest(router, "", sign("project-1", "pool-1", "", ScopeAuditRead))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", resp.Code, resp.Body.String())
	}
}
