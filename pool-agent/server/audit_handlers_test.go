package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/auditid"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
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

	artifactID   auditid.ExchangeID
	artifactName string
	artifact     *proxy.AuditArtifact
	artifactErr  error

	getID  auditid.ExchangeID
	getRow *proxy.AuditHTTPExchange
	getErr error
}

func (r *recordingAuditReader) GetHTTP(_ context.Context, sandboxID string, id auditid.ExchangeID) (*proxy.AuditHTTPExchange, error) {
	r.calls++
	r.sandboxID, r.getID = sandboxID, id
	return r.getRow, r.getErr
}

func (r *recordingAuditReader) OpenHTTPArtifact(_ context.Context, sandboxID string, id auditid.ExchangeID, artifact string) (*proxy.AuditArtifact, error) {
	r.calls++
	r.sandboxID, r.artifactID, r.artifactName = sandboxID, id, artifact
	return r.artifact, r.artifactErr
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
	resp := auditRequest(router, "?host=api.github.com&useId=use_a&since=2026-09-17T09:00:00%2B09:00&limit=5&order=asc&minStatus=400&maxStatus=499&blocked=true",
		sign("project-1", "pool-1", "", ScopeAuditRead))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	wantSince := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	if reader.query.Host != "api.github.com" || reader.query.UseID != "use_a" || reader.query.Limit != 5 || !reader.query.Since.Equal(wantSince) ||
		!reader.query.Ascending || reader.query.MinStatus != 400 || reader.query.MaxStatus != 499 || reader.query.Blocked == nil || !*reader.query.Blocked {
		t.Fatalf("proxy query = %+v, want the request's filters", reader.query)
	}
	var body struct {
		Exchanges []struct {
			ID            string    `json:"id"`
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
	if got.ID != "http_7" || got.SandboxID != "sandbox-1" || got.Status != 200 || !got.CreatedAt.Equal(createdAt) ||
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

func artifactRequest(router http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/audit/http/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func TestPoolHTTPAuditArtifactRelaysTheRecordedBody(t *testing.T) {
	reader := &recordingAuditReader{artifact: &proxy.AuditArtifact{
		Body: io.NopCloser(strings.NewReader("recorded request body")), Format: "raw", ContentType: "application/octet-stream",
	}}
	router, sign := newAuditRouter(t, reader)
	resp := artifactRequest(router, "http_42/request-body", sign("project-1", "pool-1", "sandbox-1", ScopeAuditRead))
	if resp.Code != http.StatusOK || resp.Body.String() != "recorded request body" {
		t.Fatalf("status = %d body = %q, want the recorded body", resp.Code, resp.Body.String())
	}
	if resp.Header().Get(AuditArtifactFormatHeader) != "raw" {
		t.Fatalf("format header = %q", resp.Header().Get(AuditArtifactFormatHeader))
	}
	// Narrowed to the token's sandbox, like the list.
	if reader.sandboxID != "sandbox-1" || reader.artifactID != 42 || reader.artifactName != "request-body" {
		t.Fatalf("proxy asked for sandbox %q row %s %q", reader.sandboxID, reader.artifactID, reader.artifactName)
	}
}

func TestPoolHTTPAuditArtifactAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		token      func(sign func(string, string, string, ...string) string) string
		readerErr  error
		wantStatus int
	}{
		{name: "no audit:read", path: "http_42/request-body", token: func(sign func(string, string, string, ...string) string) string {
			return sign("project-1", "pool-1", "", ScopeSandboxRead)
		}, wantStatus: http.StatusForbidden},
		{name: "query names another sandbox", path: "http_42/request-body?sandboxId=sandbox-2", token: func(sign func(string, string, string, ...string) string) string {
			return sign("project-1", "pool-1", "sandbox-1", ScopeAuditRead)
		}, wantStatus: http.StatusForbidden},
		{name: "not the token's row", path: "http_42/request-body", token: func(sign func(string, string, string, ...string) string) string {
			return sign("project-1", "pool-1", "sandbox-1", ScopeAuditRead)
		}, readerErr: proxy.ErrAuditArtifactNotFound, wantStatus: http.StatusNotFound},
		{name: "id is not an exchange id", path: "latest/request-body", token: func(sign func(string, string, string, ...string) string) string {
			return sign("project-1", "pool-1", "", ScopeAuditRead)
		}, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingAuditReader{artifactErr: tc.readerErr}
			router, sign := newAuditRouter(t, reader)
			resp := artifactRequest(router, tc.path, tc.token(sign))
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden && reader.calls != 0 {
				t.Fatal("the proxy was read for a refused request")
			}
		})
	}
}

// A discobox's own audit data is read only while it runs, and so is its
// terminal's screen or a wait on it (ADR 0137 §2). Reading must not start a
// stopped one — that would undo the stop being read about.
func TestSandboxReadsNeverStartAStoppedSandbox(t *testing.T) {
	for _, tc := range []struct{ method, route string }{
		{http.MethodGet, "/harness-hooks"},
		{http.MethodGet, "/exec-events"},
		{http.MethodGet, "/execs/exec-1/screen"},
		{http.MethodPost, "/execs/exec-1/wait"},
	} {
		route := tc.route
		t.Run(route, func(t *testing.T) {
			var reached int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached++
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(upstream.Close)
			baseURL, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			runtime := sandboxruntime.NewMemorySandboxRuntime()
			ctx := context.Background()
			if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.StopSandbox(ctx, "sandbox-1", &workerapimodel.PoolSandboxOperationRequest{}); err != nil {
				t.Fatal(err)
			}
			publicKey, sign := testPoolTokenSigner(t)
			router, err := NewRouter(Config{
				Identity:              Identity{ProjectID: "project-1", PoolID: "pool-1"},
				Runtime:               proxyTestRuntime{MemorySandboxRuntime: runtime, baseURL: baseURL},
				ControlPlanePublicKey: publicKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			call := func() *httptest.ResponseRecorder {
				req := httptest.NewRequestWithContext(ctx, tc.method, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1"+route, strings.NewReader(`{}`))
				req.Header.Set("Authorization", "Bearer "+sign("project-1", "pool-1", "sandbox-1", ScopeExecRead))
				req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
				resp := httptest.NewRecorder()
				router.ServeHTTP(resp, req)
				return resp
			}

			resp := call()
			if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "stopped") {
				t.Fatalf("stopped sandbox: status = %d body = %q, want 409 saying it is stopped", resp.Code, resp.Body.String())
			}
			sb, err := runtime.GetSandbox(ctx, "sandbox-1")
			if err != nil || sb.Status != sandboxruntime.StatusStopped {
				t.Fatalf("after the read the sandbox is %v, %v; want it still stopped", sb.Status, err)
			}
			if reached != 0 {
				t.Fatal("the read reached the sandbox agent of a stopped sandbox")
			}

			if err := runtime.StartSandbox(ctx, "sandbox-1", &workerapimodel.PoolSandboxOperationRequest{}); err != nil {
				t.Fatal(err)
			}
			if resp := call(); resp.Code != http.StatusOK || reached != 1 {
				t.Fatalf("running sandbox: status = %d, reached %d; want it proxied", resp.Code, reached)
			}
		})
	}
}

// The detail read is the whole row, including the fields a list leaves out, and
// it is narrowed exactly as the list is.
func TestPoolGetHTTPAuditRelaysTheWholeRow(t *testing.T) {
	createdAt := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	reader := &recordingAuditReader{getRow: &proxy.AuditHTTPExchange{
		ID: 7, CreatedAt: createdAt, EnqueuedAt: createdAt, WrittenAt: createdAt.Add(2 * time.Second),
		ClientID: "sandbox-1", Method: "POST", URL: "https://api.github.com/x", Host: "api.github.com", Status: 201,
		RequestHeaders:   `{"Authorization":["[REDACTED]"],"Accept":["application/json"]}`,
		ResponseHeaders:  `{"Content-Type":["application/json"]}`,
		SwappedUseIDs:    "use_a",
		AppliedHeaders:   "Authorization",
		AppliedRuleID:    "rule-1",
		CacheKey:         "key-1",
		ResponseBodyFile: "response-7", ResponseBodyFormat: "raw", ResponseBytes: 2048,
	}}
	router, sign := newAuditRouter(t, reader)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/project/project-1/pool/pool-1/audit/http/http_7", nil)
	req.Header.Set("Authorization", "Bearer "+sign("project-1", "pool-1", "sandbox-1", ScopeAuditRead))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", resp.Code, resp.Body.String())
	}
	if reader.sandboxID != "sandbox-1" || reader.getID != 7 {
		t.Fatalf("proxy asked for sandbox %q row %s", reader.sandboxID, reader.getID)
	}
	var detail struct {
		ID                   string              `json:"id"`
		WrittenAt            time.Time           `json:"writtenAt"`
		RequestHeaders       map[string][]string `json:"requestHeaders"`
		ResponseHeaders      map[string][]string `json:"responseHeaders"`
		AppliedHeaders       []string            `json:"appliedHeaders"`
		AppliedRuleID        string              `json:"appliedRuleId"`
		CacheKey             string              `json:"cacheKey"`
		ResponseBodyRecorded bool                `json:"responseBodyRecorded"`
		StreamRecorded       bool                `json:"streamRecorded"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.ID != "http_7" || !detail.WrittenAt.Equal(createdAt.Add(2*time.Second)) {
		t.Fatalf("detail = %+v, want the row's own id and write time", detail)
	}
	// Headers come back as the recorder stored them, which is already redacted.
	if got := detail.RequestHeaders["Authorization"]; len(got) != 1 || got[0] != "[REDACTED]" {
		t.Fatalf("request headers = %v, want the redacted value the recorder wrote", detail.RequestHeaders)
	}
	if len(detail.ResponseHeaders) != 1 || !reflect.DeepEqual(detail.AppliedHeaders, []string{"Authorization"}) ||
		detail.AppliedRuleID != "rule-1" || detail.CacheKey != "key-1" {
		t.Fatalf("detail lost a field a list does not carry: %+v", detail)
	}
	// What can be read beside the row, rather than the pool's file names.
	if !detail.ResponseBodyRecorded || detail.StreamRecorded {
		t.Fatalf("detail = %+v, want a recorded response body and no stream", detail)
	}
	if strings.Contains(resp.Body.String(), "response-7") {
		t.Fatalf("detail names a spool file on the pool's disk:\n%s", resp.Body.String())
	}
}

func TestPoolGetHTTPAuditRequiresAuditReadAndReportsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scope      string
		id         string
		err        error
		wantStatus int
	}{
		{name: "no audit:read", scope: ScopeSandboxRead, id: "http_7", wantStatus: http.StatusForbidden},
		{name: "not found", scope: ScopeAuditRead, id: "http_7", err: proxy.ErrAuditArtifactNotFound, wantStatus: http.StatusNotFound},
		{name: "bare row number", scope: ScopeAuditRead, id: "7", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &recordingAuditReader{getErr: tc.err, getRow: &proxy.AuditHTTPExchange{ID: 7}}
			router, sign := newAuditRouter(t, reader)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
				"/api/project/project-1/pool/pool-1/audit/http/"+tc.id, nil)
			req.Header.Set("Authorization", "Bearer "+sign("project-1", "pool-1", "", tc.scope))
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
		})
	}
}
