package agentcreds_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// fakeService is a minimal implementation, standing in for the pool-side broker
// so the wire shape can be exercised end to end without one.
type fakeService struct {
	credentials []agentcreds.Credential
	statuses    []agentcreds.RequestStatus
	gotRequest  agentcreds.RequestBody
	environment map[string]string
	statusErr   error
	trusts      []agentcreds.Trust
	gotTrust    agentcreds.TrustRequestBody
	trustStatus agentcreds.TrustRequestStatus
	gotAudit    agentcreds.AuditQuery
	exchanges   []agentcreds.AuditExchange
}

func (f *fakeService) List(context.Context) ([]agentcreds.Credential, error) {
	return f.credentials, nil
}

func (f *fakeService) Request(_ context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	f.gotRequest = body
	return agentcreds.RequestStatus{RequestID: "req-1", Status: agentcreds.StatusPending}, nil
}

func (f *fakeService) RequestStatus(context.Context, string) (agentcreds.RequestStatus, error) {
	if f.statusErr != nil {
		return agentcreds.RequestStatus{}, f.statusErr
	}
	next := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return next, nil
}

func (f *fakeService) Environment(context.Context) (map[string]string, error) {
	return f.environment, nil
}

func (f *fakeService) Trusts(context.Context) ([]agentcreds.Trust, error) {
	return f.trusts, nil
}

func (f *fakeService) RequestTrust(_ context.Context, body agentcreds.TrustRequestBody) (agentcreds.TrustRequestStatus, error) {
	f.gotTrust = body
	return f.trustStatus, nil
}

func (f *fakeService) TrustRequestStatus(context.Context, string) (agentcreds.TrustRequestStatus, error) {
	return f.trustStatus, nil
}

func (f *fakeService) AuditHTTP(_ context.Context, query agentcreds.AuditQuery) ([]agentcreds.AuditExchange, error) {
	f.gotAudit = query
	return f.exchanges, nil
}

func newTestClient(t *testing.T, svc agentcreds.Service) *agentcreds.Client {
	t.Helper()
	server := httptest.NewServer(agentcreds.NewHandler(svc))
	t.Cleanup(server.Close)
	return agentcreds.NewClient(server.URL)
}

func TestListReportsCredentialsAndNeverValues(t *testing.T) {
	svc := &fakeService{credentials: []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GITHUB_TOKEN",
		Hosts:  []string{"api.github.com"},
	}, {
		Name:    "github",
		Hosts:   []string{"api.github.com"},
		Purpose: agentcreds.PurposeDelegate,
	}}}
	credentials, err := newTestClient(t, svc).List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !reflect.DeepEqual(credentials, svc.credentials) {
		t.Fatalf("list = %#v, want %#v", credentials, svc.credentials)
	}
}

func TestRequestCarriesTheAsk(t *testing.T) {
	svc := &fakeService{}
	ask := agentcreds.RequestBody{
		Name:          "github",
		EnvVar:        "GITHUB_TOKEN",
		Hosts:         []string{"api.github.com"},
		Justification: "open a pull request",
		Purpose:       agentcreds.PurposeUse,
	}
	status, err := newTestClient(t, svc).Request(context.Background(), ask)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status.Status != agentcreds.StatusPending || !reflect.DeepEqual(svc.gotRequest, ask) {
		t.Fatalf("request = %#v, asked %#v, want %#v pending", status, svc.gotRequest, ask)
	}
}

// A refusal must survive the round trip as a refusal. The relay chain reports
// what it is told, and flattening a denial into a generic failure would make a
// revoked grant look like an outage.
func TestDenialRoundTripsAsDenial(t *testing.T) {
	svc := &fakeService{statusErr: fmt.Errorf("%w: not this sandbox's request", agentcreds.ErrDenied)}
	_, err := newTestClient(t, svc).RequestStatus(context.Background(), "req-gone")
	if !errors.Is(err, agentcreds.ErrDenied) {
		t.Fatalf("status error = %v, want ErrDenied", err)
	}
}

func TestWaitForRequestPollsUntilSettled(t *testing.T) {
	svc := &fakeService{statuses: []agentcreds.RequestStatus{
		{RequestID: "req-1", Status: agentcreds.StatusPending},
		{RequestID: "req-1", Status: agentcreds.StatusGranted, Credential: &agentcreds.Credential{Name: "github", EnvVar: "GITHUB_TOKEN"}},
	}}
	client := newTestClient(t, svc)
	status, err := client.Request(context.Background(), agentcreds.RequestBody{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"}})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status.Status != agentcreds.StatusPending {
		t.Fatalf("request returned %q, want an unsettled request", status.Status)
	}
	settled, err := client.WaitForRequest(context.Background(), status.RequestID, time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if settled.Status != agentcreds.StatusGranted || settled.Credential == nil || settled.Credential.EnvVar != "GITHUB_TOKEN" {
		t.Fatalf("settled = %#v", settled)
	}
}

// Every filter the client sends reaches the service as it was meant, and the
// limit the service sees is always one it may honor.
func TestAuditHTTPCarriesTheQueryAndSettlesTheLimit(t *testing.T) {
	since := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	blocked := true
	svc := &fakeService{exchanges: []agentcreds.AuditExchange{{ID: "http_1", URL: "https://api.github.com/", Blocked: true, BlockedReason: "route: no"}}}
	client := newTestClient(t, svc)
	exchanges, err := client.AuditHTTP(context.Background(), agentcreds.AuditQuery{Host: "api.github.com", Blocked: &blocked, Since: since, Limit: 5000})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(exchanges) != 1 || exchanges[0].BlockedReason != "route: no" {
		t.Fatalf("exchanges = %#v", exchanges)
	}
	got := svc.gotAudit
	if got.Host != "api.github.com" || got.Blocked == nil || !*got.Blocked || !got.Since.Equal(since) || got.Limit != agentcreds.MaxAuditLimit {
		t.Fatalf("service saw %+v", got)
	}

	svc.exchanges = nil
	exchanges, err = client.AuditHTTP(context.Background(), agentcreds.AuditQuery{})
	if err != nil || exchanges == nil || len(exchanges) != 0 {
		t.Fatalf("empty audit = %#v, %v; want an empty list", exchanges, err)
	}
	if svc.gotAudit.Limit != agentcreds.DefaultAuditLimit || svc.gotAudit.Blocked != nil {
		t.Fatalf("unfiltered read reached the service as %+v", svc.gotAudit)
	}
}

func TestAuditHTTPRefusesAnUnreadableQuery(t *testing.T) {
	server := httptest.NewServer(agentcreds.NewHandler(&fakeService{}))
	t.Cleanup(server.Close)
	for _, query := range []string{"blocked=maybe", "since=yesterday", "limit=0", "limit=x"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+agentcreds.PathAuditHTTP+"?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestEnvironmentReportsTheDeliveredVariables(t *testing.T) {
	svc := &fakeService{environment: map[string]string{"GH_TOKEN": "ghp_sentinel"}}
	got, err := newTestClient(t, svc).Environment(context.Background())
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	if !reflect.DeepEqual(got, svc.environment) {
		t.Fatalf("environment = %v, want %v", got, svc.environment)
	}
	empty, err := newTestClient(t, &fakeService{}).Environment(context.Background())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("environment with none = %v, %v; want an empty map", empty, err)
	}
}
